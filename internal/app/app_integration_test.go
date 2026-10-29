//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/drzbraz/deus.ai/internal/app"
	"github.com/drzbraz/deus.ai/internal/config"
)

const deadBroker = "amqp://deus_user:deus_pass@127.0.0.1:1/"

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load()
	require.NoError(t, err)

	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.DatabaseURL = envOr("TEST_DATABASE_URL", "postgres://deus:deus@localhost:55432/logistics_test?sslmode=disable")
	cfg.RabbitMQURL = envOr("TEST_RABBITMQ_URL", "amqp://deus_user:deus_pass@localhost:56720/")
	cfg.OutboxInterval = 20 * time.Millisecond
	cfg.ShutdownTimeout = 5 * time.Second
	cfg.OTelEndpoint = ""
	return cfg
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// client is a tiny JSON API client bound to a running App.
type client struct {
	t    *testing.T
	base string
}

type response struct {
	status  int
	headers http.Header
	body    []byte
}

func (r response) data(t *testing.T) map[string]any {
	t.Helper()
	var env struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(r.body, &env), string(r.body))
	return env.Data
}

func (r response) list(t *testing.T) []map[string]any {
	t.Helper()
	var env struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(r.body, &env), string(r.body))
	return env.Data
}

func (c client) call(method, path, body string, headers ...string) response {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, strings.NewReader(body))
	require.NoError(c.t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	return response{status: resp.StatusCode, headers: resp.Header, body: raw}
}

func startApp(t *testing.T, cfg config.Config) client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	application, err := app.New(ctx, cfg, zaptest.NewLogger(t, zaptest.Level(zap.WarnLevel)))
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- application.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done)
	})
	return client{t: t, base: "http://" + application.Addr()}
}

func (c client) createVessel(capacity float64) string {
	c.t.Helper()
	resp := c.call(http.MethodPost, "/api/v1/vessels",
		fmt.Sprintf(`{"name":"vessel-%s","capacity_kg":%g,"current_location":"Rotterdam"}`, uuid.NewString(), capacity))
	require.Equal(c.t, http.StatusCreated, resp.status, string(resp.body))
	return resp.data(c.t)["id"].(string)
}

func (c client) createCargo(vesselID string) string {
	c.t.Helper()
	resp := c.call(http.MethodPost, "/api/v1/cargoes", `{"name":"Coffee","weight_kg":10,"vessel_id":"`+vesselID+`"}`)
	require.Equal(c.t, http.StatusCreated, resp.status, string(resp.body))
	return resp.data(c.t)["id"].(string)
}

func TestEndToEnd_CargoLifecycleProducesEvents(t *testing.T) {
	c := startApp(t, testConfig(t))
	vesselID := c.createVessel(1000)
	cargoID := c.createCargo(vesselID)
	cargoPath := "/api/v1/cargoes/" + cargoID

	resp := c.call(http.MethodPatch, cargoPath+"/status", `{"status":"in_transit","location":"Suez"}`)
	require.Equal(t, http.StatusOK, resp.status, string(resp.body))
	resp = c.call(http.MethodPatch, cargoPath+"/status", `{"status":"delivered","location":"Singapore"}`)
	require.Equal(t, http.StatusOK, resp.status, string(resp.body))
	assert.Equal(t, `"3"`, resp.headers.Get("ETag"))

	resp = c.call(http.MethodPatch, cargoPath+"/status", `{"status":"pending"}`)
	assert.Equal(t, http.StatusConflict, resp.status)

	history := c.call(http.MethodGet, cargoPath+"/tracking", "").list(t)
	require.Len(t, history, 3)
	assert.Equal(t, []any{"Rotterdam", "Suez", "Singapore"}, []any{history[0]["location"], history[1]["location"], history[2]["location"]})

	assert.Eventually(t, func() bool {
		return len(c.call(http.MethodGet, cargoPath+"/events", "").list(t)) == 2
	}, 10*time.Second, 50*time.Millisecond, "consumer stores both status change events")

	stored := c.call(http.MethodGet, cargoPath+"/events", "").list(t)
	assert.Equal(t, "pending", stored[0]["old_status"])
	assert.Equal(t, "delivered", stored[1]["new_status"])

	vessel := c.call(http.MethodGet, "/api/v1/vessels/"+vesselID, "").data(t)
	assert.Equal(t, 0.0, vessel["load_kg"], "delivered cargo no longer uses capacity")

	ready := c.call(http.MethodGet, "/readyz", "")
	assert.Equal(t, http.StatusOK, ready.status)
	assert.JSONEq(t, `{"status":"ready","checks":{"postgres":"up","rabbitmq":"up"}}`, string(ready.body))
}

// Regression: 20 simultaneous identical POSTs with the same Idempotency-Key
// create exactly one cargo and all receive the same response.
func TestEndToEnd_ConcurrentDuplicatePostsCreateOneCargo(t *testing.T) {
	c := startApp(t, testConfig(t))
	vesselID := c.createVessel(1000)
	body := `{"name":"Fragile","weight_kg":5,"vessel_id":"` + vesselID + `"}`
	key := uuid.NewString()

	const clients = 20
	responses := make([]response, clients)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			responses[i] = c.call(http.MethodPost, "/api/v1/cargoes", body, "Idempotency-Key", key)
		}()
	}
	close(start)
	wg.Wait()

	for _, resp := range responses {
		assert.Equal(t, http.StatusCreated, resp.status, string(resp.body))
		assert.True(t, bytes.Equal(responses[0].body, resp.body), "every duplicate gets the identical response")
	}
	cargoes := c.call(http.MethodGet, "/api/v1/cargoes?vessel_id="+vesselID, "").list(t)
	assert.Len(t, cargoes, 1)

	reused := c.call(http.MethodPost, "/api/v1/cargoes", strings.Replace(body, "Fragile", "Other", 1), "Idempotency-Key", key)
	assert.Equal(t, http.StatusUnprocessableEntity, reused.status)
}

func TestEndToEnd_OptimisticConcurrencyWithIfMatch(t *testing.T) {
	c := startApp(t, testConfig(t))
	cargoPath := "/api/v1/cargoes/" + c.createCargo(c.createVessel(100))

	etag := c.call(http.MethodGet, cargoPath, "").headers.Get("ETag")
	require.Equal(t, `"1"`, etag)

	first := c.call(http.MethodPatch, cargoPath+"/status", `{"status":"in_transit"}`, "If-Match", etag)
	require.Equal(t, http.StatusOK, first.status)

	stale := c.call(http.MethodPatch, cargoPath+"/status", `{"status":"delivered"}`, "If-Match", etag)
	assert.Equal(t, http.StatusPreconditionFailed, stale.status)
}

// Regression: with RabbitMQ down the API keeps accepting status changes; the
// events wait in the outbox and are delivered exactly once when it returns.
func TestEndToEnd_BrokerOutageLosesNoEvents(t *testing.T) {
	cfg := testConfig(t)
	var cargoPath string

	t.Run("broker down", func(t *testing.T) {
		down := cfg
		down.RabbitMQURL = deadBroker
		c := startApp(t, down)
		cargoPath = "/api/v1/cargoes/" + c.createCargo(c.createVessel(100))

		resp := c.call(http.MethodPatch, cargoPath+"/status", `{"status":"in_transit"}`)
		assert.Equal(t, http.StatusOK, resp.status, "writes succeed while the broker is down")

		ready := c.call(http.MethodGet, "/readyz", "")
		assert.Equal(t, http.StatusOK, ready.status)
		assert.Contains(t, string(ready.body), `"status":"degraded"`)
		assert.Contains(t, string(ready.body), `"rabbitmq":"down"`)

		time.Sleep(200 * time.Millisecond) // the relay retries and fails meanwhile
		assert.Empty(t, c.call(http.MethodGet, cargoPath+"/events", "").list(t))
	})

	t.Run("broker back", func(t *testing.T) {
		c := startApp(t, cfg)

		assert.Eventually(t, func() bool {
			return len(c.call(http.MethodGet, cargoPath+"/events", "").list(t)) == 1
		}, 15*time.Second, 50*time.Millisecond)

		time.Sleep(300 * time.Millisecond)
		assert.Len(t, c.call(http.MethodGet, cargoPath+"/events", "").list(t), 1, "delivered exactly once")
	})
}

// Regression: when Postgres disappears at runtime, clients get 503 with
// Retry-After (not 500) and the readiness probe takes the instance out.
func TestEndToEnd_DatabaseOutageReturns503(t *testing.T) {
	cfg := testConfig(t)
	dbURL, err := url.Parse(cfg.DatabaseURL)
	require.NoError(t, err)
	proxy := newTCPProxy(t, dbURL.Host)
	dbURL.Host = proxy.addr()
	cfg.DatabaseURL = dbURL.String()
	cfg.RequestTimeout = 2 * time.Second
	cfg.RelayEnabled, cfg.ConsumerEnabled = false, false
	c := startApp(t, cfg)
	vesselPath := "/api/v1/vessels/" + c.createVessel(100)

	proxy.cut()

	resp := c.call(http.MethodGet, vesselPath, "")
	assert.Equal(t, http.StatusServiceUnavailable, resp.status, string(resp.body))
	assert.Equal(t, "5", resp.headers.Get("Retry-After"))

	assert.Equal(t, http.StatusServiceUnavailable, c.call(http.MethodGet, "/readyz", "").status)
	assert.Equal(t, http.StatusOK, c.call(http.MethodGet, "/healthz", "").status, "liveness must not depend on the database")
}

func TestNew_FailsWhenDatabaseIsUnreachable(t *testing.T) {
	cfg := testConfig(t)
	cfg.DatabaseURL = "postgres://deus:deus@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"
	cfg.DBConnectAttempts = 2

	_, err := app.New(context.Background(), cfg, zap.NewNop())

	assert.ErrorContains(t, err, "connect to database")
}

// tcpProxy forwards connections to target until cut() simulates an outage.
type tcpProxy struct {
	t        *testing.T
	listener net.Listener
	target   string

	mu    sync.Mutex
	conns []net.Conn
	down  bool
}

func newTCPProxy(t *testing.T, target string) *tcpProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &tcpProxy{t: t, listener: ln, target: target}
	go p.accept()
	t.Cleanup(func() { _ = ln.Close(); p.cut() })
	return p
}

func (p *tcpProxy) addr() string { return p.listener.Addr().String() }

func (p *tcpProxy) accept() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.down {
			p.mu.Unlock()
			_ = client.Close()
			continue
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			p.mu.Unlock()
			_ = client.Close()
			continue
		}
		p.conns = append(p.conns, client, server)
		p.mu.Unlock()

		go func() { _, _ = io.Copy(server, client); _ = server.Close() }()
		go func() { _, _ = io.Copy(client, server); _ = client.Close() }()
	}
}

func (p *tcpProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = true
	for _, conn := range p.conns {
		_ = conn.Close()
	}
	p.conns = nil
}
