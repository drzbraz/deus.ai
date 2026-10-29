//go:build integration

package rabbitmq_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/drzbraz/deus.ai/internal/backoff"
	"github.com/drzbraz/deus.ai/internal/events"
	"github.com/drzbraz/deus.ai/internal/events/rabbitmq"
)

func brokerURL() string {
	if url := os.Getenv("TEST_RABBITMQ_URL"); url != "" {
		return url
	}
	return "amqp://deus_user:deus_pass@localhost:56720/"
}

// recordingHandler captures payloads and answers with a fixed decision.
type recordingHandler struct {
	mu       sync.Mutex
	decision events.Decision
	payloads [][]byte
}

func (h *recordingHandler) Handle(_ context.Context, payload []byte) events.Decision {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.payloads = append(h.payloads, payload)
	return h.decision
}

func (h *recordingHandler) received(body string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.payloads {
		if string(p) == body {
			return true
		}
	}
	return false
}

func startConsumer(t *testing.T, handler rabbitmq.MessageHandler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	consumer := rabbitmq.NewConsumer(brokerURL(), handler, zap.NewNop(), backoff.Default())
	go func() { consumer.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func publish(t *testing.T, publisher *rabbitmq.Publisher, body string) {
	t.Helper()
	err := publisher.Publish(context.Background(), events.OutboxMessage{
		ID: uuid.New(), RoutingKey: events.RoutingKeyStatusChanged, Payload: []byte(body),
	})
	require.NoError(t, err)
}

func TestPublishAndConsume(t *testing.T) {
	publisher := rabbitmq.NewPublisher(brokerURL())
	t.Cleanup(func() { _ = publisher.Close() })
	require.NoError(t, publisher.Check(context.Background()))

	handler := &recordingHandler{decision: events.Ack}
	startConsumer(t, handler)

	body := `{"marker":"` + uuid.NewString() + `"}`
	publish(t, publisher, body)

	assert.Eventually(t, func() bool { return handler.received(body) }, 10*time.Second, 20*time.Millisecond)
}

// Regression: a poison message ends in the dead-letter queue instead of
// being redelivered forever.
func TestRejectedMessagesAreDeadLettered(t *testing.T) {
	publisher := rabbitmq.NewPublisher(brokerURL())
	t.Cleanup(func() { _ = publisher.Close() })
	handler := &recordingHandler{decision: events.Reject}
	startConsumer(t, handler)

	body := `{"poison":"` + uuid.NewString() + `"}`
	publish(t, publisher, body)

	assert.Eventually(t, func() bool { return dlqContains(t, body) }, 10*time.Second, 50*time.Millisecond)
}

// Regression: with the broker unreachable, publishing fails fast with an
// error (the relay keeps the message in the outbox) instead of hanging.
func TestPublisher_BrokerUnreachable(t *testing.T) {
	publisher := rabbitmq.NewPublisher("amqp://deus_user:deus_pass@127.0.0.1:1/")

	err := publisher.Publish(context.Background(), events.OutboxMessage{ID: uuid.New(), RoutingKey: events.RoutingKeyStatusChanged})
	assert.Error(t, err)
	assert.Error(t, publisher.Check(context.Background()))
}

func TestPublisher_ReconnectsAfterConnectionLoss(t *testing.T) {
	publisher := rabbitmq.NewPublisher(brokerURL())
	t.Cleanup(func() { _ = publisher.Close() })
	publish(t, publisher, `{}`)

	require.NoError(t, publisher.Close()) // simulates a dropped connection

	publish(t, publisher, `{}`)
}

func TestConsumer_StopsWhileBrokerUnreachable(t *testing.T) {
	consumer := rabbitmq.NewConsumer("amqp://deus_user:deus_pass@127.0.0.1:1/", &recordingHandler{}, zap.NewNop(), backoff.None{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	consumer.Run(ctx) // must return once ctx is done, despite never connecting
}

func dlqContains(t *testing.T, body string) bool {
	t.Helper()
	conn, err := amqp.Dial(brokerURL())
	require.NoError(t, err)
	defer conn.Close()
	ch, err := conn.Channel()
	require.NoError(t, err)

	for {
		msg, ok, err := ch.Get(rabbitmq.DeadLetterQueue, true)
		if err != nil || !ok {
			return false
		}
		if string(msg.Body) == body {
			return true
		}
	}
}
