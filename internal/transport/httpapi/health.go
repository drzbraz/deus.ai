package httpapi

import (
	"context"
	"net/http"
	"sync"
	"time"
)

const healthCheckTimeout = 2 * time.Second

// HealthCheck probes one dependency for /readyz.
type HealthCheck struct {
	Name string
	// Critical checks make the instance "not ready" when they fail. The
	// broker is not critical: thanks to the outbox the API keeps accepting
	// writes while RabbitMQ is down, so we report it as degraded instead.
	Critical bool
	Check    func(ctx context.Context) error
}

type healthHandler struct {
	responder
	checks []HealthCheck
}

type readinessResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// live answers whether the process is running; it never checks dependencies,
// otherwise an orchestrator would restart healthy pods during a DB outage.
func (h healthHandler) live(w http.ResponseWriter, _ *http.Request) {
	h.write(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ready answers whether this instance should receive traffic.
func (h healthHandler) ready(w http.ResponseWriter, r *http.Request) {
	resp := readinessResponse{Status: "ready", Checks: map[string]string{}}
	status := http.StatusOK

	for i, err := range runChecks(r.Context(), h.checks) {
		check := h.checks[i]
		if err != nil {
			resp.Checks[check.Name] = "down"
			if check.Critical {
				resp.Status, status = "unavailable", http.StatusServiceUnavailable
			} else if resp.Status == "ready" {
				resp.Status = "degraded"
			}
			continue
		}
		resp.Checks[check.Name] = "up"
	}

	h.write(w, status, resp)
}

// runChecks probes every dependency concurrently, so the probe takes as long
// as the slowest check instead of the sum of all of them. With Postgres and
// RabbitMQ both down, a sequential probe would take 2x the check timeout and
// exceed a typical orchestrator probe timeout.
func runChecks(ctx context.Context, checks []HealthCheck) []error {
	errs := make([]error, len(checks)) // one slot per goroutine: no lock needed
	var wg sync.WaitGroup
	for i, check := range checks {
		wg.Go(func() { errs[i] = runCheck(ctx, check) })
	}
	wg.Wait()
	return errs
}

func runCheck(ctx context.Context, check HealthCheck) error {
	ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	return check.Check(ctx)
}
