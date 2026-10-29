package events

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/drzbraz/deus.ai/internal/backoff"
)

const tracerName = "github.com/drzbraz/deus.ai/internal/events"

// OutboxStore is the relay's view of the outbox table.
type OutboxStore interface {
	FetchUnpublished(ctx context.Context, limit int) ([]OutboxMessage, error)
	MarkPublished(ctx context.Context, id uuid.UUID, at time.Time) error
	MarkFailed(ctx context.Context, id uuid.UUID, reason string) error
}

// Publisher delivers one message to the broker and returns only once the
// broker has confirmed it (or failed).
type Publisher interface {
	Publish(ctx context.Context, msg OutboxMessage) error
}

type TxManager interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

type RelayMetrics interface {
	EventPublished(ctx context.Context)
	EventPublishFailed(ctx context.Context)
}

// Relay moves events from the outbox to the broker. Delivery is
// at-least-once: if the process dies between the broker confirm and the
// commit, the message is published again and the consumer deduplicates it.
type Relay struct {
	tx        TxManager
	store     OutboxStore
	publisher Publisher
	metrics   RelayMetrics
	log       *zap.Logger

	interval  time.Duration
	batchSize int
	backoff   backoff.Policy
	now       func() time.Time
}

type RelayOption func(*Relay)

func WithInterval(d time.Duration) RelayOption { return func(r *Relay) { r.interval = d } }
func WithBatchSize(n int) RelayOption          { return func(r *Relay) { r.batchSize = n } }
func WithBackoff(p backoff.Policy) RelayOption { return func(r *Relay) { r.backoff = p } }

func NewRelay(tx TxManager, store OutboxStore, publisher Publisher, metrics RelayMetrics, log *zap.Logger, opts ...RelayOption) *Relay {
	r := &Relay{
		tx:        tx,
		store:     store,
		publisher: publisher,
		metrics:   metrics,
		log:       log,
		interval:  500 * time.Millisecond,
		batchSize: 100,
		backoff:   backoff.Default(),
		now:       func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run publishes until ctx is cancelled. While the broker or database is
// down it backs off exponentially instead of hammering them.
func (r *Relay) Run(ctx context.Context) {
	failures := 0
	for {
		published, err := r.PublishBatch(ctx)
		if ctx.Err() != nil {
			return
		}

		wait := r.interval
		switch {
		case err != nil:
			failures++
			wait = r.backoff.Delay(failures)
			r.log.Warn("outbox relay failed, backing off",
				zap.Error(err), zap.Int("consecutive_failures", failures), zap.Duration("retry_in", wait))
		case published == r.batchSize:
			failures, wait = 0, 0 // a full batch means a backlog: keep draining
		default:
			failures = 0
		}

		if backoff.Sleep(ctx, wait) != nil {
			return
		}
	}
}

// PublishBatch publishes the oldest pending messages in order. It stops at
// the first failure so events of the same cargo are not reordered.
func (r *Relay) PublishBatch(ctx context.Context) (published int, err error) {
	var publishErr error

	txErr := r.tx.WithinTx(ctx, func(ctx context.Context) error {
		messages, err := r.store.FetchUnpublished(ctx, r.batchSize)
		if err != nil {
			return err
		}

		for _, msg := range messages {
			if err := r.publish(ctx, msg); err != nil {
				publishErr = fmt.Errorf("publish event %s: %w", msg.ID, err)
				return r.store.MarkFailed(ctx, msg.ID, err.Error())
			}
			if err := r.store.MarkPublished(ctx, msg.ID, r.now()); err != nil {
				return err
			}
			published++
		}
		return nil
	})
	if txErr != nil {
		return 0, fmt.Errorf("relay outbox batch: %w", txErr)
	}
	return published, publishErr
}

// publish continues the trace that was started by the HTTP request which
// created the event, so Jaeger shows request -> publish -> consume.
func (r *Relay) publish(ctx context.Context, msg OutboxMessage) error {
	parent := otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(msg.Headers))
	ctx, span := otel.Tracer(tracerName).Start(parent, "publish "+msg.RoutingKey,
		trace.WithSpanKind(trace.SpanKindProducer))
	defer span.End()

	if err := r.publisher.Publish(ctx, msg); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish failed")
		r.metrics.EventPublishFailed(ctx)
		return err
	}

	r.metrics.EventPublished(ctx)
	return nil
}
