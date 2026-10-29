package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/drzbraz/deus.ai/internal/domain"
)

// MeterName scopes every custom instrument of this service.
const MeterName = "github.com/drzbraz/deus.ai"

// Metrics holds the business and event pipeline instruments. HTTP latency is
// recorded separately by the otelhttp middleware.
type Metrics struct {
	transitions     metric.Int64Counter
	published       metric.Int64Counter
	publishFailures metric.Int64Counter
	consumed        metric.Int64Counter
	consumeDuration metric.Float64Histogram
}

func NewMetrics(meter metric.Meter) (*Metrics, error) {
	var m Metrics
	var errs [5]error

	m.transitions, errs[0] = meter.Int64Counter("cargo.status_transitions",
		metric.WithDescription("Committed cargo status transitions"))
	m.published, errs[1] = meter.Int64Counter("events.published",
		metric.WithDescription("Events confirmed by the broker"))
	m.publishFailures, errs[2] = meter.Int64Counter("events.publish_failures",
		metric.WithDescription("Events the broker did not accept; they stay in the outbox"))
	m.consumed, errs[3] = meter.Int64Counter("events.consumed",
		metric.WithDescription("Events handled by the consumer, by outcome"))
	m.consumeDuration, errs[4] = meter.Float64Histogram("event.consume.duration",
		metric.WithUnit("s"), metric.WithDescription("Time to handle one consumed event"))

	if err := errors.Join(errs[:]...); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Metrics) RecordTransition(ctx context.Context, from, to domain.Status) {
	m.transitions.Add(ctx, 1, metric.WithAttributes(
		attribute.String("from", string(from)),
		attribute.String("to", string(to)),
	))
}

func (m *Metrics) EventPublished(ctx context.Context) {
	m.published.Add(ctx, 1)
}

func (m *Metrics) EventPublishFailed(ctx context.Context) {
	m.publishFailures.Add(ctx, 1)
}

func (m *Metrics) EventConsumed(ctx context.Context, outcome string, took time.Duration) {
	attrs := metric.WithAttributes(attribute.String("outcome", outcome))
	m.consumed.Add(ctx, 1, attrs)
	m.consumeDuration.Record(ctx, took.Seconds(), attrs)
}

// RegisterOutboxBacklog exposes the number of unpublished events as a gauge.
// A growing backlog is the first signal that the broker is unreachable.
func RegisterOutboxBacklog(meter metric.Meter, countPending func(context.Context) (int64, error)) error {
	_, err := meter.Int64ObservableGauge("outbox.pending",
		metric.WithDescription("Events waiting in the outbox to be published"),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			pending, err := countPending(ctx)
			if err != nil {
				return err
			}
			o.Observe(pending)
			return nil
		}))
	if err != nil {
		return fmt.Errorf("register outbox.pending gauge: %w", err)
	}
	return nil
}
