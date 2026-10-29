// Package logger builds the structured zap logger and enriches it with
// request and trace correlation fields.
package logger

import (
	"context"
	"fmt"

	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New creates a JSON production logger with the given level.
func New(level string) (*zap.Logger, error) {
	parsed, err := zapcore.ParseLevel(level)
	if err != nil {
		return nil, fmt.Errorf("parse log level: %w", err)
	}

	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(parsed)
	cfg.EncoderConfig.TimeKey = "time"
	cfg.EncoderConfig.EncodeTime = zapcore.RFC3339NanoTimeEncoder

	log, err := cfg.Build()
	if err != nil {
		return nil, fmt.Errorf("build logger: %w", err)
	}
	return log, nil
}

// WithContext returns log annotated with the request id and trace/span ids
// found in ctx, so a log line can be matched to its trace in Jaeger.
func WithContext(ctx context.Context, log *zap.Logger) *zap.Logger {
	fields := make([]zap.Field, 0, 3)

	if requestID := middleware.GetReqID(ctx); requestID != "" {
		fields = append(fields, zap.String("request_id", requestID))
	}
	if spanCtx := trace.SpanContextFromContext(ctx); spanCtx.IsValid() {
		fields = append(fields,
			zap.String("trace_id", spanCtx.TraceID().String()),
			zap.String("span_id", spanCtx.SpanID().String()),
		)
	}

	return log.With(fields...)
}
