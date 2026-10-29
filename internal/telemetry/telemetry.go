// Package telemetry configures OpenTelemetry tracing and metrics.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap"
)

const metricExportInterval = 10 * time.Second

// LatencyBuckets (seconds) give useful p50/p95/p99 resolution for an API
// whose requests should take between 1ms and a few seconds.
var LatencyBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type Config struct {
	// Endpoint of the OTLP gRPC collector, e.g. http://otel-collector:4317.
	// When empty, telemetry stays disabled (no-op) and nothing is exported.
	Endpoint    string
	ServiceName string
}

// Shutdown flushes buffered telemetry and stops the exporters.
type Shutdown func(ctx context.Context) error

// Setup installs the global tracer and meter providers. Exporting is
// asynchronous and failures are only logged: an unavailable collector must
// never slow down or fail a business request.
func Setup(ctx context.Context, cfg Config, log *zap.Logger) (Shutdown, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	if cfg.Endpoint == "" {
		log.Info("telemetry export disabled: OTEL_EXPORTER_OTLP_ENDPOINT is not set")
		return func(context.Context) error { return nil }, nil
	}

	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Warn("telemetry export failed", zap.Error(err))
	}))

	res := resource.NewSchemaless(attribute.String("service.name", cfg.ServiceName))

	traceExporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpointURL(cfg.Endpoint))
	if err != nil {
		return nil, fmt.Errorf("create trace exporter: %w", err)
	}
	metricExporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpointURL(cfg.Endpoint))
	if err != nil {
		return nil, fmt.Errorf("create metric exporter: %w", err)
	}

	tracerProvider := sdktrace.NewTracerProvider(
		// The batcher uses a bounded queue and drops spans when it is full.
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)
	meterProvider := NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(metricExportInterval))),
		sdkmetric.WithResource(res),
	)

	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)

	return func(ctx context.Context) error {
		return errors.Join(tracerProvider.Shutdown(ctx), meterProvider.Shutdown(ctx))
	}, nil
}

// NewMeterProvider applies the latency bucket view to every duration
// histogram. Tests use it with a ManualReader.
func NewMeterProvider(opts ...sdkmetric.Option) *sdkmetric.MeterProvider {
	durationView := sdkmetric.NewView(
		sdkmetric.Instrument{Name: "*duration*", Kind: sdkmetric.InstrumentKindHistogram},
		sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{Boundaries: LatencyBuckets}},
	)
	return sdkmetric.NewMeterProvider(append(opts, sdkmetric.WithView(durationView))...)
}
