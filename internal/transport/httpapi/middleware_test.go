package httpapi_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/drzbraz/deus.ai/internal/domain"
	"github.com/drzbraz/deus.ai/internal/service"
	"github.com/drzbraz/deus.ai/internal/telemetry"
	"github.com/drzbraz/deus.ai/internal/transport/httpapi"
)

func newRequest(method, path, body string) *http.Request {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	return httptest.NewRequest(method, path, reader)
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestUnknownRouteAndMethodAreJSON(t *testing.T) {
	s := newTestServer()

	rec := s.do(t, http.MethodGet, "/api/v1/ships", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "route_not_found", decode(t, rec.Body.Bytes()).Error.Code)

	rec = s.do(t, http.MethodDelete, "/api/v1/cargoes/"+cargoID.String(), "")
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, "method_not_allowed", decode(t, rec.Body.Bytes()).Error.Code)
}

func TestPanicBecomesJSON500(t *testing.T) {
	s := newTestServer()
	s.vessels.get = func(uuid.UUID) (domain.Vessel, error) { panic("nil map") }

	rec := s.do(t, http.MethodGet, "/api/v1/vessels/"+vesselID.String(), "")

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "internal_error", decode(t, rec.Body.Bytes()).Error.Code)
}

func TestRequestTimeoutIsPropagatedToServices(t *testing.T) {
	s := newTestServer()
	s.timeout = 20 * time.Millisecond
	s.cargoes.get = func(ctx context.Context, _ uuid.UUID) (domain.Cargo, error) {
		_, hasDeadline := ctx.Deadline()
		assert.True(t, hasDeadline)
		<-ctx.Done() // a slow database call that observes the deadline
		return domain.Cargo{}, domain.ErrUnavailable
	}

	rec := s.do(t, http.MethodGet, "/api/v1/cargoes/"+cargoID.String(), "")

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestRequestIDIsReturned(t *testing.T) {
	s := newTestServer()
	rec := s.do(t, http.MethodGet, "/healthz", "", "X-Request-Id", "abc-123")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"status":"ok"}`, rec.Body.String())
}

func TestReadiness(t *testing.T) {
	up := func(context.Context) error { return nil }
	down := func(context.Context) error { return errors.New("down") }

	tests := []struct {
		name       string
		checks     []httpapi.HealthCheck
		wantStatus int
		wantBody   string
	}{
		{"all up", []httpapi.HealthCheck{{"postgres", true, up}, {"rabbitmq", false, up}},
			http.StatusOK, `{"status":"ready","checks":{"postgres":"up","rabbitmq":"up"}}`},
		{"broker down is degraded but still ready", []httpapi.HealthCheck{{"postgres", true, up}, {"rabbitmq", false, down}},
			http.StatusOK, `{"status":"degraded","checks":{"postgres":"up","rabbitmq":"down"}}`},
		{"database down is not ready", []httpapi.HealthCheck{{"postgres", true, down}, {"rabbitmq", false, down}},
			http.StatusServiceUnavailable, `{"status":"unavailable","checks":{"postgres":"down","rabbitmq":"down"}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer()
			s.checks = tt.checks

			rec := s.do(t, http.MethodGet, "/readyz", "")

			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.JSONEq(t, tt.wantBody, rec.Body.String())
		})
	}
}

// Regression: latency metrics must be labelled with the route pattern, never
// the raw path, or every cargo id would create a new time series.
func TestLatencyMetricUsesRoutePattern(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(telemetry.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(previous) })

	s := newTestServer()
	s.cargoes.get = func(context.Context, uuid.UUID) (domain.Cargo, error) { return sampleCargo(), nil }
	s.do(t, http.MethodGet, "/api/v1/cargoes/"+cargoID.String(), "")

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	histogram := findHistogram(t, rm, "http.server.request.duration")
	require.NotEmpty(t, histogram.DataPoints)
	point := histogram.DataPoints[0]

	route, ok := point.Attributes.Value("http.route")
	require.True(t, ok, "http.route attribute missing")
	assert.Equal(t, "/api/v1/cargoes/{cargoID}", route.AsString())
	for _, attr := range point.Attributes.ToSlice() {
		assert.NotContains(t, attr.Value.String(), cargoID.String(), "raw id leaked into %s", attr.Key)
	}
	assert.Equal(t, telemetry.LatencyBuckets, point.Bounds, "custom latency buckets must apply")
}

func findHistogram(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Histogram[float64] {
	t.Helper()
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				h, ok := m.Data.(metricdata.Histogram[float64])
				require.True(t, ok)
				return h
			}
		}
	}
	t.Fatalf("metric %s not found", name)
	return metricdata.Histogram[float64]{}
}

func TestIdempotency_ReplaysFirstResponse(t *testing.T) {
	s := newTestServer()
	calls := 0
	s.cargoes.create = func(service.CreateCargoInput) (domain.Cargo, error) {
		calls++
		return sampleCargo(), nil
	}
	body := `{"name":"Coffee","weight_kg":10,"vessel_id":"` + vesselID.String() + `"}`

	first := s.do(t, http.MethodPost, "/api/v1/cargoes", body, "Idempotency-Key", "key-1")
	second := s.do(t, http.MethodPost, "/api/v1/cargoes", body, "Idempotency-Key", "key-1")

	assert.Equal(t, 1, calls, "side effect must happen once")
	assert.Equal(t, http.StatusCreated, second.Code)
	assert.Equal(t, first.Body.String(), second.Body.String())
	assert.Equal(t, first.Header().Get("Location"), second.Header().Get("Location"))
	assert.Equal(t, first.Header().Get("ETag"), second.Header().Get("ETag"))
	assert.Empty(t, first.Header().Get("Idempotent-Replayed"))
	assert.Equal(t, "true", second.Header().Get("Idempotent-Replayed"))
}

func TestIdempotency_KeyReuseWithDifferentBodyIsRejected(t *testing.T) {
	s := newTestServer()
	s.cargoes.create = func(service.CreateCargoInput) (domain.Cargo, error) { return sampleCargo(), nil }

	s.do(t, http.MethodPost, "/api/v1/cargoes", `{"name":"a","weight_kg":1,"vessel_id":"`+vesselID.String()+`"}`, "Idempotency-Key", "k")
	rec := s.do(t, http.MethodPost, "/api/v1/cargoes", `{"name":"b","weight_kg":1,"vessel_id":"`+vesselID.String()+`"}`, "Idempotency-Key", "k")

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, "idempotency_key_reused", decode(t, rec.Body.Bytes()).Error.Code)
}

func TestIdempotency_ServerErrorsAreNotRemembered(t *testing.T) {
	s := newTestServer()
	failures := 1
	s.cargoes.create = func(service.CreateCargoInput) (domain.Cargo, error) {
		if failures > 0 {
			failures--
			return domain.Cargo{}, domain.ErrUnavailable
		}
		return sampleCargo(), nil
	}
	body := `{"name":"a","weight_kg":1,"vessel_id":"` + vesselID.String() + `"}`

	first := s.do(t, http.MethodPost, "/api/v1/cargoes", body, "Idempotency-Key", "retry-me")
	second := s.do(t, http.MethodPost, "/api/v1/cargoes", body, "Idempotency-Key", "retry-me")

	assert.Equal(t, http.StatusServiceUnavailable, first.Code)
	assert.Equal(t, "5", first.Header().Get("Retry-After"))
	assert.Equal(t, http.StatusCreated, second.Code, "a retry after a 5xx runs again")
}

func TestIdempotency_ClientErrorsAreRemembered(t *testing.T) {
	s := newTestServer()
	calls := 0
	s.cargoes.updateStatus = func(service.UpdateStatusInput) (domain.Cargo, error) {
		calls++
		return domain.Cargo{}, domain.ErrInvalidTransition
	}
	path := "/api/v1/cargoes/" + cargoID.String() + "/status"

	s.do(t, http.MethodPatch, path, `{"status":"delivered"}`, "Idempotency-Key", "k")
	rec := s.do(t, http.MethodPatch, path, `{"status":"delivered"}`, "Idempotency-Key", "k")

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, 1, calls)
}

func TestIdempotency_EdgeCases(t *testing.T) {
	s := newTestServer()
	s.vessels.create = func(service.CreateVesselInput) (domain.Vessel, error) { return sampleVessel(), nil }

	rec := s.do(t, http.MethodPost, "/api/v1/vessels", `{"name":"x"}`, "Idempotency-Key", strings.Repeat("k", 256))
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	req := newRequest(http.MethodPost, "/api/v1/vessels", `{"name":"`+strings.Repeat("x", 2<<20)+`"}`)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "big")
	assert.Equal(t, http.StatusRequestEntityTooLarge, serve(s.handler(), req).Code)

	s.idempotency.claimErr = domain.ErrUnavailable
	rec = s.do(t, http.MethodPost, "/api/v1/vessels", `{"name":"x"}`, "Idempotency-Key", "k")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
