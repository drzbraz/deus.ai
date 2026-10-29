package events_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/drzbraz/deus.ai/internal/backoff"
	"github.com/drzbraz/deus.ai/internal/domain"
	"github.com/drzbraz/deus.ai/internal/events"
)

var occurredAt = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func sampleEvent() domain.StatusChanged {
	return domain.StatusChanged{
		EventID: uuid.New(), CargoID: uuid.New(),
		OldStatus: domain.StatusPending, NewStatus: domain.StatusInTransit, OccurredAt: occurredAt,
	}
}

func TestStatusChangedMessage_RoundTrip(t *testing.T) {
	event := sampleEvent()

	payload, err := events.EncodeStatusChanged(event)
	require.NoError(t, err)
	assert.JSONEq(t, `{"event_id":"`+event.EventID.String()+`","cargo_id":"`+event.CargoID.String()+`",
		"old_status":"pending","new_status":"in_transit","timestamp":"2026-10-29T12:00:00Z"}`, string(payload))

	decoded, err := events.DecodeStatusChanged(payload)
	require.NoError(t, err)
	assert.Equal(t, event, decoded)
}

func TestDecodeStatusChanged_RejectsMalformedMessages(t *testing.T) {
	id := uuid.NewString()
	for name, payload := range map[string]string{
		"not json":       `{`,
		"missing ids":    `{"old_status":"pending","new_status":"in_transit","timestamp":"2026-10-29T12:00:00Z"}`,
		"unknown status": `{"event_id":"` + id + `","cargo_id":"` + id + `","old_status":"lost","new_status":"in_transit","timestamp":"2026-10-29T12:00:00Z"}`,
		"no timestamp":   `{"event_id":"` + id + `","cargo_id":"` + id + `","old_status":"pending","new_status":"in_transit"}`,
	} {
		_, err := events.DecodeStatusChanged([]byte(payload))
		assert.Error(t, err, name)
	}
}

// --- relay ---

type passthroughTx struct{}

func (passthroughTx) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

type fakeOutbox struct {
	mu        sync.Mutex
	pending   []events.OutboxMessage
	published []uuid.UUID
	failed    map[uuid.UUID]int
	fetchErr  error
}

func (f *fakeOutbox) FetchUnpublished(_ context.Context, limit int) ([]events.OutboxMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	var out []events.OutboxMessage
	for _, msg := range f.pending {
		if len(out) == limit {
			break
		}
		if !contains(f.published, msg.ID) {
			out = append(out, msg)
		}
	}
	return out, nil
}

func (f *fakeOutbox) MarkPublished(_ context.Context, id uuid.UUID, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, id)
	return nil
}

func (f *fakeOutbox) MarkFailed(_ context.Context, id uuid.UUID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed == nil {
		f.failed = map[uuid.UUID]int{}
	}
	f.failed[id]++
	return nil
}

func (f *fakeOutbox) publishedIDs() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uuid.UUID(nil), f.published...)
}

// fakeBroker fails the first `failures` publishes, simulating an outage.
type fakeBroker struct {
	mu       sync.Mutex
	failures int
	received []uuid.UUID
}

func (b *fakeBroker) Publish(_ context.Context, msg events.OutboxMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failures > 0 {
		b.failures--
		return errors.New("connection refused")
	}
	b.received = append(b.received, msg.ID)
	return nil
}

type metricsSpy struct {
	mu                         sync.Mutex
	published, publishFailures int
	consumed                   []string
}

func (m *metricsSpy) EventPublished(context.Context) { m.mu.Lock(); m.published++; m.mu.Unlock() }
func (m *metricsSpy) EventPublishFailed(context.Context) {
	m.mu.Lock()
	m.publishFailures++
	m.mu.Unlock()
}
func (m *metricsSpy) EventConsumed(_ context.Context, outcome string, _ time.Duration) {
	m.mu.Lock()
	m.consumed = append(m.consumed, outcome)
	m.mu.Unlock()
}

func outboxWith(n int) *fakeOutbox {
	f := &fakeOutbox{}
	for range n {
		f.pending = append(f.pending, events.OutboxMessage{ID: uuid.New(), RoutingKey: events.RoutingKeyStatusChanged})
	}
	return f
}

func TestRelay_PublishesInOrderAndMarksPublished(t *testing.T) {
	outbox, broker, metrics := outboxWith(3), &fakeBroker{}, &metricsSpy{}
	relay := events.NewRelay(passthroughTx{}, outbox, broker, metrics, zap.NewNop())

	published, err := relay.PublishBatch(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 3, published)
	assert.Equal(t, ids(outbox.pending), broker.received)
	assert.Equal(t, ids(outbox.pending), outbox.publishedIDs())
	assert.Equal(t, 3, metrics.published)
}

// Regression: when the broker is down nothing is marked published and the
// batch stops at the first failure, preserving order.
func TestRelay_BrokerDownLeavesMessagesPending(t *testing.T) {
	outbox, broker, metrics := outboxWith(3), &fakeBroker{failures: 1}, &metricsSpy{}
	relay := events.NewRelay(passthroughTx{}, outbox, broker, metrics, zap.NewNop())

	published, err := relay.PublishBatch(context.Background())

	assert.Error(t, err)
	assert.Zero(t, published)
	assert.Empty(t, outbox.publishedIDs())
	assert.Equal(t, 1, outbox.failed[outbox.pending[0].ID])
	assert.Empty(t, broker.received, "later messages must not overtake the failed one")
	assert.Equal(t, 1, metrics.publishFailures)
}

func TestRelay_DatabaseErrorIsReported(t *testing.T) {
	outbox := &fakeOutbox{fetchErr: domain.ErrUnavailable}
	relay := events.NewRelay(passthroughTx{}, outbox, &fakeBroker{}, &metricsSpy{}, zap.NewNop())

	_, err := relay.PublishBatch(context.Background())

	assert.ErrorIs(t, err, domain.ErrUnavailable)
}

func TestRelay_RunRecoversAfterOutageAndDrainsBacklog(t *testing.T) {
	outbox, broker := outboxWith(5), &fakeBroker{failures: 3}
	relay := events.NewRelay(passthroughTx{}, outbox, broker, &metricsSpy{}, zap.NewNop(),
		events.WithInterval(time.Millisecond), events.WithBatchSize(2), events.WithBackoff(backoff.None{}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { relay.Run(ctx); close(done) }()

	assert.Eventually(t, func() bool { return len(outbox.publishedIDs()) == 5 }, 2*time.Second, time.Millisecond)
	cancel()
	<-done
	assert.Equal(t, ids(outbox.pending), outbox.publishedIDs(), "order is preserved across the outage")
}

// --- consumer handler ---

type fakeStore struct {
	mu       sync.Mutex
	saved    map[uuid.UUID]bool
	errs     []error // returned in order before succeeding
	attempts int
}

func (s *fakeStore) Save(_ context.Context, e domain.StatusChanged) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		return false, err
	}
	if s.saved == nil {
		s.saved = map[uuid.UUID]bool{}
	}
	if s.saved[e.EventID] {
		return false, nil
	}
	s.saved[e.EventID] = true
	return true, nil
}

func encoded(t *testing.T, e domain.StatusChanged) []byte {
	t.Helper()
	payload, err := events.EncodeStatusChanged(e)
	require.NoError(t, err)
	return payload
}

func TestHandler_StoresEventAndAcks(t *testing.T) {
	store, metrics := &fakeStore{}, &metricsSpy{}
	h := events.NewHandler(store, metrics, zap.NewNop(), backoff.None{})

	assert.Equal(t, events.Ack, h.Handle(context.Background(), encoded(t, sampleEvent())))
	assert.Equal(t, []string{events.OutcomeStored}, metrics.consumed)
}

// Regression: at-least-once delivery means the same event can arrive twice.
func TestHandler_DuplicateDeliveryIsStoredOnce(t *testing.T) {
	store, metrics := &fakeStore{}, &metricsSpy{}
	h := events.NewHandler(store, metrics, zap.NewNop(), backoff.None{})
	payload := encoded(t, sampleEvent())

	assert.Equal(t, events.Ack, h.Handle(context.Background(), payload))
	assert.Equal(t, events.Ack, h.Handle(context.Background(), payload))

	assert.Len(t, store.saved, 1)
	assert.Equal(t, []string{events.OutcomeStored, events.OutcomeDuplicate}, metrics.consumed)
}

func TestHandler_PoisonMessagesAreDeadLettered(t *testing.T) {
	tests := map[string]struct {
		payload []byte
		errs    []error
	}{
		"malformed json": {payload: []byte(`{"broken`)},
		"unknown cargo":  {payload: encoded(t, sampleEvent()), errs: []error{domain.ErrNotFound}},
		"unexpected db":  {payload: encoded(t, sampleEvent()), errs: []error{errors.New("check constraint")}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			metrics := &metricsSpy{}
			h := events.NewHandler(&fakeStore{errs: tt.errs}, metrics, zap.NewNop(), backoff.None{})

			assert.Equal(t, events.Reject, h.Handle(context.Background(), tt.payload))
			assert.Equal(t, []string{events.OutcomeRejected}, metrics.consumed)
		})
	}
}

// Regression: a database outage must not dead-letter healthy events.
func TestHandler_RetriesWhileDatabaseIsUnavailable(t *testing.T) {
	store := &fakeStore{errs: []error{domain.ErrUnavailable, domain.ErrUnavailable, domain.ErrUnavailable}}
	h := events.NewHandler(store, &metricsSpy{}, zap.NewNop(), backoff.None{})

	assert.Equal(t, events.Ack, h.Handle(context.Background(), encoded(t, sampleEvent())))
	assert.Equal(t, 4, store.attempts)
}

func TestHandler_RequeuesWhenShuttingDownDuringOutage(t *testing.T) {
	store := &fakeStore{errs: []error{domain.ErrUnavailable}}
	h := events.NewHandler(store, &metricsSpy{}, zap.NewNop(), backoff.Exponential{Initial: time.Hour, Max: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	assert.Equal(t, events.Requeue, h.Handle(ctx, encoded(t, sampleEvent())))
}

func ids(messages []events.OutboxMessage) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(messages))
	for _, m := range messages {
		out = append(out, m.ID)
	}
	return out
}

func contains(list []uuid.UUID, id uuid.UUID) bool {
	for _, item := range list {
		if item == id {
			return true
		}
	}
	return false
}
