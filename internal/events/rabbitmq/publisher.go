package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"

	"github.com/drzbraz/deus.ai/internal/events"
)

const confirmTimeout = 5 * time.Second

var errNacked = errors.New("broker rejected the message")

// Publisher implements events.Publisher with publisher confirms, so a message
// counts as published only once RabbitMQ has persisted it. The connection is
// opened lazily and reopened after any failure, so the relay recovers by
// itself when the broker comes back.
type Publisher struct {
	url string

	mu   sync.Mutex
	conn *amqp.Connection
	ch   *amqp.Channel
}

func NewPublisher(url string) *Publisher {
	return &Publisher{url: url}
}

func (p *Publisher) Publish(ctx context.Context, msg events.OutboxMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	ch, err := p.channel()
	if err != nil {
		return err
	}

	headers := amqp.Table{}
	otel.GetTextMapPropagator().Inject(ctx, headerCarrier(headers))

	confirmation, err := ch.PublishWithDeferredConfirmWithContext(ctx, Exchange, msg.RoutingKey, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    msg.ID.String(),
		Timestamp:    time.Now().UTC(),
		Headers:      headers,
		Body:         msg.Payload,
	})
	if err != nil {
		p.reset()
		return fmt.Errorf("publish to rabbitmq: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, confirmTimeout)
	defer cancel()
	acked, err := confirmation.WaitContext(ctx)
	if err != nil {
		p.reset()
		return fmt.Errorf("wait for broker confirm: %w", err)
	}
	if !acked {
		return errNacked
	}
	return nil
}

// Check reports whether the broker is reachable, for the readiness probe.
func (p *Publisher) Check(context.Context) error {
	// A publish in progress already proves (or is testing) the connection;
	// do not make the probe wait behind it.
	if !p.mu.TryLock() {
		return nil
	}
	defer p.mu.Unlock()

	_, err := p.channel()
	return err
}

func (p *Publisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reset()
	return nil
}

// channel returns an open confirm-mode channel, reconnecting if needed.
// Callers must hold p.mu.
func (p *Publisher) channel() (*amqp.Channel, error) {
	if p.ch != nil && !p.ch.IsClosed() {
		return p.ch, nil
	}
	p.reset()

	conn, err := dial(p.url)
	if err != nil {
		return nil, err
	}
	ch, err := openConfirmChannel(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	p.conn, p.ch = conn, ch
	return ch, nil
}

func openConfirmChannel(conn *amqp.Connection) (*amqp.Channel, error) {
	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("open channel: %w", err)
	}
	if err := declareTopology(ch); err != nil {
		return nil, err
	}
	if err := ch.Confirm(false); err != nil {
		return nil, fmt.Errorf("enable publisher confirms: %w", err)
	}
	return ch, nil
}

func (p *Publisher) reset() {
	if p.conn != nil {
		_ = p.conn.Close()
	}
	p.conn, p.ch = nil, nil
}
