package logger_test

import (
	"context"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/drzbraz/deus.ai/internal/logger"
)

func TestNew(t *testing.T) {
	log, err := logger.New("debug")
	require.NoError(t, err)
	assert.True(t, log.Core().Enabled(zap.DebugLevel))

	_, err = logger.New("loud")
	assert.Error(t, err)
}

func TestWithContext_AddsCorrelationFields(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID})

	ctx := context.WithValue(context.Background(), middleware.RequestIDKey, "req-1")
	ctx = trace.ContextWithSpanContext(ctx, spanCtx)

	logger.WithContext(ctx, zap.New(core)).Info("hello")

	fields := logs.All()[0].ContextMap()
	assert.Equal(t, "req-1", fields["request_id"])
	assert.Equal(t, traceID.String(), fields["trace_id"])
	assert.Equal(t, spanID.String(), fields["span_id"])
}

func TestWithContext_NoCorrelationAvailable(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)

	logger.WithContext(context.Background(), zap.New(core)).Info("hello")

	assert.Empty(t, logs.All()[0].ContextMap())
}
