package events

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/drzbraz/deus.ai/internal/backoff"
	"github.com/drzbraz/deus.ai/internal/domain"
	"github.com/drzbraz/deus.ai/internal/logger"
)

// Decision tells the broker adapter what to do with a delivery.
type Decision int

const (
	// Ack removes the message: it was stored (or already had been).
	Ack Decision = iota
	// Reject dead-letters the message: retrying it can never succeed.
	Reject
	// Requeue returns the message to the queue: we are shutting down.
	Requeue
)

// Outcomes reported to metrics.
const (
	OutcomeStored    = "stored"
	OutcomeDuplicate = "duplicate"
	OutcomeRejected  = "rejected"
	OutcomeRequeued  = "requeued"
)

// EventStore persists consumed events idempotently.
type EventStore interface {
	Save(ctx context.Context, event domain.StatusChanged) (inserted bool, err error)
}

type ConsumerMetrics interface {
	EventConsumed(ctx context.Context, outcome string, took time.Duration)
}

// Handler holds the consumer's business logic, independent of the broker,
// so every delivery outcome can be unit tested.
type Handler struct {
	store   EventStore
	metrics ConsumerMetrics
	log     *zap.Logger
	backoff backoff.Policy
}

func NewHandler(store EventStore, metrics ConsumerMetrics, log *zap.Logger, policy backoff.Policy) *Handler {
	return &Handler{store: store, metrics: metrics, log: log, backoff: policy}
}

// Handle stores one status change event. While the database is unavailable
// it keeps retrying the same message instead of requeueing it: requeues count
// towards the queue's delivery limit and would dead-letter healthy messages
// during a long outage.
func (h *Handler) Handle(ctx context.Context, payload []byte) Decision {
	started := time.Now()
	ctx, span := otel.Tracer(tracerName).Start(ctx, "consume "+RoutingKeyStatusChanged,
		trace.WithSpanKind(trace.SpanKindConsumer))
	defer span.End()
	log := logger.WithContext(ctx, h.log)

	decision, outcome := h.handle(ctx, log, payload)
	h.metrics.EventConsumed(ctx, outcome, time.Since(started))
	return decision
}

func (h *Handler) handle(ctx context.Context, log *zap.Logger, payload []byte) (Decision, string) {
	event, err := DecodeStatusChanged(payload)
	if err != nil {
		log.Error("malformed event, dead-lettering", zap.Error(err), zap.ByteString("payload", payload))
		return Reject, OutcomeRejected
	}

	log = log.With(
		zap.Stringer("event_id", event.EventID),
		zap.Stringer("cargo_id", event.CargoID),
		zap.String("old_status", string(event.OldStatus)),
		zap.String("new_status", string(event.NewStatus)),
		zap.Time("occurred_at", event.OccurredAt),
	)

	inserted, err := h.saveWithRetry(ctx, log, event)
	switch {
	case err == nil && inserted:
		log.Info("cargo status event stored")
		return Ack, OutcomeStored
	case err == nil:
		log.Info("duplicate cargo status event ignored")
		return Ack, OutcomeDuplicate
	case ctx.Err() != nil:
		log.Info("shutting down, requeueing event")
		return Requeue, OutcomeRequeued
	default:
		log.Error("event cannot be stored, dead-lettering", zap.Error(err))
		return Reject, OutcomeRejected
	}
}

func (h *Handler) saveWithRetry(ctx context.Context, log *zap.Logger, event domain.StatusChanged) (bool, error) {
	for attempt := 1; ; attempt++ {
		inserted, err := h.store.Save(ctx, event)
		if !errors.Is(err, domain.ErrUnavailable) {
			return inserted, err
		}

		delay := h.backoff.Delay(attempt)
		log.Warn("database unavailable, retrying event", zap.Int("attempt", attempt), zap.Duration("retry_in", delay), zap.Error(err))
		if sleepErr := backoff.Sleep(ctx, delay); sleepErr != nil {
			return false, sleepErr
		}
	}
}
