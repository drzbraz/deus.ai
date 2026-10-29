package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/drzbraz/deus.ai/internal/logger"
)

// accessLog writes one structured line per request. Probe endpoints are
// logged at debug level so they do not drown real traffic.
func accessLog(base *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			level := zapcore.InfoLevel
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				level = zapcore.DebugLevel
			}
			logger.WithContext(r.Context(), base).Log(level, "http request",
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.String("route", routePattern(r)),
				zap.Int("status", ww.Status()),
				zap.Int("bytes", ww.BytesWritten()),
				zap.Duration("duration", time.Since(started)),
			)
		})
	}
}

// recoverer turns a panic into a JSON 500 instead of a dropped connection.
func recoverer(rs responder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { //nolint:contextcheck // rs.error reads the request context from r
				if rec := recover(); rec != nil {
					if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
						panic(rec)
					}
					logger.WithContext(r.Context(), rs.log).Error("panic recovered",
						zap.Any("panic", rec), zap.ByteString("stack", debug.Stack()))
					rs.error(w, r, fmt.Errorf("panic: %v", rec))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// requestTimeout bounds how long a request may hold database connections and
// locks. Storage calls observe the deadline and fail with 503 when it passes.
func requestTimeout(timeout time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// tagRoute labels the span and the latency metric with the route *pattern*
// (/api/v1/cargoes/{cargoID}) once chi has matched it. Using the raw path
// would create one time series per cargo id and explode metric cardinality.
func tagRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)

		pattern := routePattern(r)
		if pattern == "" {
			return
		}
		route := attribute.String("http.route", pattern)

		span := trace.SpanFromContext(r.Context())
		span.SetName(r.Method + " " + pattern)
		span.SetAttributes(route)

		if labeler, ok := otelhttp.LabelerFromContext(r.Context()); ok {
			labeler.Add(route)
		}
	})
}

func routePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		return rctx.RoutePattern()
	}
	return ""
}
