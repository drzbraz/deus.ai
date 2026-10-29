package testkit

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
)

// Broker contract: names other services rely on. Declared here on purpose
// so that renaming them in the code breaks the suite.
const (
	EventsExchange      = "cargo.events"
	StatusChangedKey    = "cargo.status_changed"
	DeadLetterQueueName = "cargo.status_changed.dlq"
)

// CountRows runs a COUNT(*) style query against the verification database.
func CountRows(t testing.TB, db *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

// StatusChangedPayload builds a wire-format event, for injecting messages
// directly into the broker.
func StatusChangedPayload(t testing.TB, eventID, cargoID, oldStatus, newStatus string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"event_id": eventID, "cargo_id": cargoID,
		"old_status": oldStatus, "new_status": newStatus,
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	})
	require.NoError(t, err)
	return raw
}

// PublishRaw publishes a message to the events exchange, bypassing the API,
// and waits for the broker confirm.
func PublishRaw(t testing.TB, url string, body []byte) {
	t.Helper()
	conn, err := amqp.Dial(url)
	require.NoError(t, err)
	defer conn.Close()
	ch, err := conn.Channel()
	require.NoError(t, err)
	require.NoError(t, ch.Confirm(false))

	confirm, err := ch.PublishWithDeferredConfirmWithContext(context.Background(), EventsExchange, StatusChangedKey, false, false,
		amqp.Publishing{ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: uuid.NewString(), Body: body})
	require.NoError(t, err)
	acked, err := confirm.WaitContext(context.Background())
	require.NoError(t, err)
	require.True(t, acked, "broker must confirm the message")
}

// TapEvents binds a private queue to the events exchange so a test can see
// exactly what the API publishes on the wire.
func TapEvents(t testing.TB, url string) <-chan amqp.Delivery {
	t.Helper()
	conn, err := amqp.Dial(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	require.NoError(t, err)

	require.NoError(t, ch.ExchangeDeclare(EventsExchange, amqp.ExchangeTopic, true, false, false, false, nil))
	q, err := ch.QueueDeclare("", false, true, true, false, nil)
	require.NoError(t, err)
	require.NoError(t, ch.QueueBind(q.Name, StatusChangedKey, EventsExchange, false, nil))
	deliveries, err := ch.Consume(q.Name, "", true, true, false, false, nil)
	require.NoError(t, err)
	return deliveries
}

// DeadLettered reports whether a message with this exact body sits in the
// dead-letter queue. The scan does not consume: unacked messages return to
// the queue when the channel closes.
func DeadLettered(t testing.TB, url string, body []byte) bool {
	t.Helper()
	conn, err := amqp.Dial(url)
	require.NoError(t, err)
	defer conn.Close()
	ch, err := conn.Channel()
	require.NoError(t, err)
	defer ch.Close()

	for {
		msg, ok, err := ch.Get(DeadLetterQueueName, false)
		if err != nil || !ok {
			return false
		}
		if string(msg.Body) == string(body) {
			return true
		}
	}
}

// Proxy forwards TCP traffic to a dependency so a test can cut and restore
// the network path, simulating an outage of a running database or broker.
type Proxy struct {
	listener net.Listener
	target   string

	mu    sync.Mutex
	conns []net.Conn
	down  bool
}

func NewProxy(t testing.TB, target string) *Proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &Proxy{listener: ln, target: target}
	go p.accept()
	t.Cleanup(func() { _ = ln.Close(); p.Cut() })
	return p
}

func (p *Proxy) Addr() string { return p.listener.Addr().String() }

// Cut drops every open connection and refuses new ones.
func (p *Proxy) Cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = true
	for _, conn := range p.conns {
		_ = conn.Close()
	}
	p.conns = nil
}

// Restore accepts connections again.
func (p *Proxy) Restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = false
}

func (p *Proxy) accept() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.forward(client)
	}
}

func (p *Proxy) forward(client net.Conn) {
	p.mu.Lock()
	if p.down {
		p.mu.Unlock()
		_ = client.Close()
		return
	}
	p.mu.Unlock()

	server, err := net.DialTimeout("tcp", p.target, 2*time.Second)
	if err != nil {
		_ = client.Close()
		return
	}

	p.mu.Lock()
	if p.down {
		p.mu.Unlock()
		_ = client.Close()
		_ = server.Close()
		return
	}
	p.conns = append(p.conns, client, server)
	p.mu.Unlock()

	go func() { _, _ = io.Copy(server, client); _ = server.Close() }()
	go func() { _, _ = io.Copy(client, server); _ = client.Close() }()
}
