package rabbitmq

import (
	"context"
	"errors"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"github.com/drzbraz/deus.ai/internal/backoff"
	"github.com/drzbraz/deus.ai/internal/events"
)

// fakeAcknowledger records what the consumer told the broker.
type fakeAcknowledger struct {
	calls []string
	err   error
}

func (f *fakeAcknowledger) Ack(uint64, bool) error { f.calls = append(f.calls, "ack"); return f.err }
func (f *fakeAcknowledger) Nack(_ uint64, _ bool, requeue bool) error {
	f.calls = append(f.calls, map[bool]string{true: "nack-requeue", false: "nack-drop"}[requeue])
	return f.err
}
func (f *fakeAcknowledger) Reject(uint64, bool) error {
	f.calls = append(f.calls, "reject")
	return f.err
}

type decidingHandler struct {
	decision events.Decision
	calls    int
}

func (h *decidingHandler) Handle(context.Context, []byte) events.Decision {
	h.calls++
	return h.decision
}

func TestProcess_MapsDecisionsToBrokerAcks(t *testing.T) {
	tests := map[events.Decision]string{
		events.Ack:     "ack",
		events.Reject:  "reject",
		events.Requeue: "nack-requeue",
	}
	for decision, want := range tests {
		ack := &fakeAcknowledger{}
		c := NewConsumer("", &decidingHandler{decision: decision}, zap.NewNop(), backoff.None{})

		c.process(context.Background(), amqp.Delivery{Acknowledger: ack})

		assert.Equal(t, []string{want}, ack.calls)
	}
}

// Regression: on shutdown queued messages go straight back to the queue
// without running the handler (no DB calls against a closing pool).
func TestProcess_ShuttingDownRequeuesWithoutHandling(t *testing.T) {
	ack := &fakeAcknowledger{}
	handler := &decidingHandler{decision: events.Ack}
	c := NewConsumer("", handler, zap.NewNop(), backoff.None{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c.process(ctx, amqp.Delivery{Acknowledger: ack})

	assert.Zero(t, handler.calls)
	assert.Equal(t, []string{"nack-requeue"}, ack.calls)
}

func TestProcess_AckFailureIsOnlyLogged(t *testing.T) {
	ack := &fakeAcknowledger{err: errors.New("channel closed")}
	c := NewConsumer("", &decidingHandler{decision: events.Ack}, zap.NewNop(), backoff.None{})

	c.process(context.Background(), amqp.Delivery{Acknowledger: ack}) // must not panic

	assert.Equal(t, []string{"ack"}, ack.calls)
}

func TestOrderingKey(t *testing.T) {
	withHeader := amqp.Delivery{MessageId: "m1", Headers: amqp.Table{headerAggregateID: "cargo-7"}}
	without := amqp.Delivery{MessageId: "m2"}
	emptyHeader := amqp.Delivery{MessageId: "m3", Headers: amqp.Table{headerAggregateID: ""}}

	assert.Equal(t, "cargo-7", orderingKey(withHeader))
	assert.Equal(t, "m2", orderingKey(without), "messages from other publishers are spread by id")
	assert.Equal(t, "m3", orderingKey(emptyHeader))
}

func TestWithWorkers_HasAFloorOfOne(t *testing.T) {
	assert.Equal(t, 1, NewConsumer("", nil, zap.NewNop(), backoff.None{}, WithWorkers(0)).workers)
	assert.Equal(t, 8, NewConsumer("", nil, zap.NewNop(), backoff.None{}, WithWorkers(8)).workers)
	assert.Equal(t, DefaultWorkers, NewConsumer("", nil, zap.NewNop(), backoff.None{}).workers)
}
