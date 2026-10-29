package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.uber.org/zap"

	"github.com/drzbraz/deus.ai/internal/idempotency"
)

// Dependencies is everything the HTTP layer needs. It is assembled in the
// composition root (internal/app).
type Dependencies struct {
	Vessels          VesselService
	Cargoes          CargoService
	Tracking         TrackingService
	Tx               TxManager
	IdempotencyStore idempotency.Store
	HealthChecks     []HealthCheck
	Logger           *zap.Logger
	RequestTimeout   time.Duration
}

// NewRouter builds the HTTP handler. Middleware order matters:
// otelhttp (outermost) starts the span and latency timer, so everything else,
// including logs, carries the trace id.
func NewRouter(d Dependencies) http.Handler {
	rs := responder{log: d.Logger}
	vessels := vesselHandler{rs, d.Vessels}
	cargoes := cargoHandler{rs, d.Cargoes}
	tracking := trackingHandler{rs, d.Tracking}
	health := healthHandler{rs, d.HealthChecks}
	guard := idempotencyGuard{rs, d.Tx, d.IdempotencyStore}

	r := chi.NewRouter()
	r.Use(
		middleware.RequestID,
		tagRoute,
		accessLog(d.Logger),
		recoverer(rs),
		requestTimeout(d.RequestTimeout),
	)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) { rs.error(w, r, errRouteNotFound) })
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) { rs.error(w, r, errMethodNotAllowed) })

	r.Get("/healthz", health.live)
	r.Get("/readyz", health.ready)

	r.Route("/api/v1", func(r chi.Router) {
		r.Route("/vessels", func(r chi.Router) {
			r.With(guard.protect("create_vessel")).Post("/", vessels.create)
			r.Get("/", vessels.list)
			r.Get("/{vesselID}", vessels.get)
			r.Patch("/{vesselID}/location", vessels.updateLocation)
		})

		r.Route("/cargoes", func(r chi.Router) {
			r.With(guard.protect("create_cargo")).Post("/", cargoes.create)
			r.Get("/", cargoes.list)
			r.Get("/{cargoID}", cargoes.get)
			r.With(guard.protect("update_cargo_status")).Patch("/{cargoID}/status", cargoes.updateStatus)
			r.With(guard.protect("record_movement")).Post("/{cargoID}/tracking", tracking.recordMovement)
			r.Get("/{cargoID}/tracking", tracking.history)
			r.Get("/{cargoID}/events", tracking.events)
		})
	})

	return otelhttp.NewHandler(r, "http.server")
}
