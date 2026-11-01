//go:build integration

package rabbitmq_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
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

// startConsumer runs a consumer until the test ends: t.Context() is cancelled
// just before cleanup functions run, so the cleanup only waits for it to stop.
func startConsumer(t *testing.T, handler rabbitmq.MessageHandler) {
	t.Helper()
	done := make(chan struct{})
	consumer := rabbitmq.NewConsumer(brokerURL(), handler, zap.NewNop(), backoff.Default())
	go func() { consumer.Run(t.Context()); close(done) }()
	t.Cleanup(func() { <-done })
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

// orderingHandler records, per cargo, the order in which events were handled,
// and the highest number of events handled at the same time.
type orderingHandler struct {
	marker  string
	mu      sync.Mutex
	order   map[string][]int
	running atomic.Int32
	peak    atomic.Int32
}

func (h *orderingHandler) Handle(_ context.Context, payload []byte) events.Decision {
	var msg struct {
		Marker string `json:"marker"`
		Cargo  string `json:"cargo"`
		Seq    int    `json:"seq"`
	}
	if json.Unmarshal(payload, &msg) != nil || msg.Marker != h.marker {
		return events.Ack // another test's message
	}

	now := h.running.Add(1)
	for peak := h.peak.Load(); now > peak && !h.peak.CompareAndSwap(peak, now); peak = h.peak.Load() {
	}
	time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond)
	h.running.Add(-1)

	h.mu.Lock()
	h.order[msg.Cargo] = append(h.order[msg.Cargo], msg.Seq)
	h.mu.Unlock()
	return events.Ack
}

func (h *orderingHandler) handled() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, seqs := range h.order {
		n += len(seqs)
	}
	return n
}

// Regression: the consumer processes different cargoes in parallel but never
// reorders the events of one cargo.
func TestConsumer_ParallelAcrossCargoesOrderedWithinCargo(t *testing.T) {
	publisher := rabbitmq.NewPublisher(brokerURL())
	t.Cleanup(func() { _ = publisher.Close() })
	handler := &orderingHandler{marker: uuid.NewString(), order: map[string][]int{}}

	const cargoes, perCargo = 20, 10
	ids := make([]uuid.UUID, cargoes)
	for i := range ids {
		ids[i] = uuid.New()
	}
	for seq := range perCargo {
		for _, id := range ids {
			body := fmt.Sprintf(`{"marker":%q,"cargo":%q,"seq":%d}`, handler.marker, id, seq)
			require.NoError(t, publisher.Publish(context.Background(), events.OutboxMessage{
				ID: uuid.New(), AggregateID: id, RoutingKey: events.RoutingKeyStatusChanged, Payload: []byte(body),
			}))
		}
	}

	consumer := rabbitmq.NewConsumer(brokerURL(), handler, zap.NewNop(), backoff.Default(), rabbitmq.WithWorkers(8))
	done := make(chan struct{})
	go func() { consumer.Run(t.Context()); close(done) }()
	t.Cleanup(func() { <-done })

	require.Eventually(t, func() bool { return handler.handled() == cargoes*perCargo }, 30*time.Second, 20*time.Millisecond)

	handler.mu.Lock()
	defer handler.mu.Unlock()
	for cargo, seqs := range handler.order {
		for i, seq := range seqs {
			require.Equal(t, i, seq, "cargo %s handled out of order: %v", cargo, seqs)
		}
	}
	assert.Greater(t, handler.peak.Load(), int32(1), "events of different cargoes must be handled in parallel")
}

// countingHandler records which of its messages were acknowledged.
type countingHandler struct {
	marker  string
	work    func(ctx context.Context)
	mu      sync.Mutex
	handled map[string]bool
}

func (h *countingHandler) Handle(ctx context.Context, payload []byte) events.Decision {
	var msg struct {
		Marker string `json:"marker"`
		ID     string `json:"id"`
	}
	if json.Unmarshal(payload, &msg) != nil || msg.Marker != h.marker {
		return events.Ack
	}
	h.work(ctx)
	if ctx.Err() != nil {
		return events.Requeue
	}
	h.mu.Lock()
	h.handled[msg.ID] = true
	h.mu.Unlock()
	return events.Ack
}

func (h *countingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.handled)
}

func publishMarked(t *testing.T, marker string, n int) {
	t.Helper()
	publisher := rabbitmq.NewPublisher(brokerURL())
	defer publisher.Close()
	for i := range n {
		body := fmt.Sprintf(`{"marker":%q,"id":"%d"}`, marker, i)
		require.NoError(t, publisher.Publish(context.Background(), events.OutboxMessage{
			ID: uuid.New(), AggregateID: uuid.New(), RoutingKey: events.RoutingKeyStatusChanged, Payload: []byte(body),
		}))
	}
}

// runningConsumer is a consumer a test can stop at a chosen moment.
type runningConsumer struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// runConsumer starts a consumer that runs until stop is called or the test ends.
func runConsumer(t *testing.T, handler rabbitmq.MessageHandler) *runningConsumer {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	rc := &runningConsumer{cancel: cancel, done: make(chan struct{})}
	go func() {
		rabbitmq.NewConsumer(brokerURL(), handler, zap.NewNop(), backoff.Default(), rabbitmq.WithWorkers(4)).Run(ctx)
		close(rc.done)
	}()
	t.Cleanup(func() { rc.stop() })
	return rc
}

// stop shuts the consumer down and reports how long that took. It is safe to call twice.
func (rc *runningConsumer) stop() time.Duration {
	started := time.Now()
	rc.cancel()
	<-rc.done
	return time.Since(started)
}

// Regression: stopping the consumer in the middle of a backlog neither
// deadlocks nor loses messages; what was not handled is redelivered.
func TestConsumer_ShutdownUnderLoadLosesNothing(t *testing.T) {
	const total = 120
	marker := uuid.NewString()
	publishMarked(t, marker, total)

	first := &countingHandler{marker: marker, handled: map[string]bool{},
		work: func(context.Context) { time.Sleep(5 * time.Millisecond) }}
	consumer := runConsumer(t, first)
	require.Eventually(t, func() bool { return first.count() >= 20 }, 15*time.Second, 5*time.Millisecond)
	assert.Less(t, consumer.stop(), 5*time.Second, "shutdown must not hang")
	require.Less(t, first.count(), total, "the test must stop mid-backlog")

	second := &countingHandler{marker: marker, handled: map[string]bool{}, work: func(context.Context) {}}
	runConsumer(t, second)
	require.Eventually(t, func() bool {
		missing := 0
		for i := range total {
			id := fmt.Sprint(i)
			first.mu.Lock()
			second.mu.Lock()
			if !first.handled[id] && !second.handled[id] {
				missing++
			}
			second.mu.Unlock()
			first.mu.Unlock()
		}
		return missing == 0
	}, 20*time.Second, 20*time.Millisecond, "every message is handled by one of the two consumers")
}

// Regression: workers stuck in a slow dependency (e.g. the database is down)
// must not block shutdown.
func TestConsumer_ShutdownWithStuckWorkers(t *testing.T) {
	marker := uuid.NewString()
	publishMarked(t, marker, 30)

	stuck := make(chan struct{}, 30)
	handler := &countingHandler{marker: marker, handled: map[string]bool{}, work: func(ctx context.Context) {
		stuck <- struct{}{}
		<-ctx.Done() // blocked until shutdown, like a DB call that never returns
	}}
	consumer := runConsumer(t, handler)
	select {
	case <-stuck: // with random cargo ids not every worker is guaranteed a message
	case <-time.After(15 * time.Second):
		t.Fatal("workers never started")
	}
	time.Sleep(100 * time.Millisecond) // let the other workers pick up theirs

	assert.Less(t, consumer.stop(), 5*time.Second, "stuck workers must be released by shutdown")
	assert.Zero(t, handler.count())

	drain := &countingHandler{marker: marker, handled: map[string]bool{}, work: func(context.Context) {}}
	runConsumer(t, drain)
	require.Eventually(t, func() bool { return drain.count() == 30 }, 20*time.Second, 20*time.Millisecond,
		"requeued messages are redelivered")
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
