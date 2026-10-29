package telemetry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.uber.org/zap"

	"github.com/drzbraz/deus.ai/internal/domain"
	"github.com/drzbraz/deus.ai/internal/telemetry"
)

func TestSetup_DisabledWithoutEndpoint(t *testing.T) {
	shutdown, err := telemetry.Setup(context.Background(), telemetry.Config{}, zap.NewNop())

	require.NoError(t, err)
	assert.NoError(t, shutdown(context.Background()))
}

// Regression: an unreachable collector must not fail startup or shutdown hard.
func TestSetup_UnreachableCollectorDoesNotBlock(t *testing.T) {
	shutdown, err := telemetry.Setup(context.Background(),
		telemetry.Config{Endpoint: "http://127.0.0.1:1", ServiceName: "test"}, zap.NewNop())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	_ = shutdown(ctx) // the flush fails; we only care that it is bounded
	assert.Less(t, time.Since(started), 2*time.Second, "shutdown must respect its deadline")
}

func TestMetrics_RecordsBusinessAndPipelineInstruments(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meter := telemetry.NewMeterProvider(sdkmetric.WithReader(reader)).Meter(telemetry.MeterName)
	metrics, err := telemetry.NewMetrics(meter)
	require.NoError(t, err)
	require.NoError(t, telemetry.RegisterOutboxBacklog(meter, func(context.Context) (int64, error) { return 7, nil }))

	ctx := context.Background()
	metrics.RecordTransition(ctx, domain.StatusPending, domain.StatusInTransit)
	metrics.EventPublished(ctx)
	metrics.EventPublishFailed(ctx)
	metrics.EventConsumed(ctx, "stored", 30*time.Millisecond)

	names := collectNames(t, reader)
	for _, name := range []string{
		"cargo.status_transitions", "events.published", "events.publish_failures",
		"events.consumed", "event.consume.duration", "outbox.pending",
	} {
		assert.Contains(t, names, name)
	}
}

func TestOutboxBacklog_CallbackErrorIsReported(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meter := telemetry.NewMeterProvider(sdkmetric.WithReader(reader)).Meter(telemetry.MeterName)
	require.NoError(t, telemetry.RegisterOutboxBacklog(meter, func(context.Context) (int64, error) {
		return 0, errors.New("db down")
	}))

	var rm metricdata.ResourceMetrics
	assert.Error(t, reader.Collect(context.Background(), &rm))
}

func collectNames(t *testing.T, reader *sdkmetric.ManualReader) []string {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	var names []string
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			names = append(names, m.Name)
		}
	}
	return names
}
