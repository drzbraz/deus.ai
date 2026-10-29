//go:build e2e

// Package resilience checks how the system behaves when its dependencies
// misbehave: outages, slow locks, poison messages, crashes and shutdowns.
// Each test boots its own in-process API so it can control the failure.
package resilience

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/drzbraz/deus.ai/internal/app"
	"github.com/drzbraz/deus.ai/internal/config"
	. "github.com/drzbraz/deus.ai/tests/testkit"
)

const deadBroker = "amqp://deus_user:deus_pass@127.0.0.1:1/"

var db *pgxpool.Pool

func TestMain(m *testing.M) {
	if LoadEnv().External() {
		// These tests break dependencies on purpose; never against a shared deployment.
		os.Exit(0)
	}
	var err error
	db, err = pgxpool.New(context.Background(), LoadEnv().DatabaseURL)
	if err != nil {
		panic(err)
	}
	code := m.Run()
	db.Close()
	os.Exit(code)
}

func env() Env { return LoadEnv() }

func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	return u.Host
}

func withHost(t *testing.T, rawURL, host string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	u.Host = host
	return u.String()
}

func pendingOutbox(t *testing.T, cargoID string) int {
	return CountRows(t, db, `SELECT COUNT(*) FROM outbox WHERE aggregate_id = $1 AND published_at IS NULL`, cargoID)
}

// --- message broker -------------------------------------------------------

func TestBrokerDownAtStartup_WritesSucceedAndEventsArriveLater(t *testing.T) {
	var cargoID string

	t.Run("while the broker is unreachable", func(t *testing.T) {
		c := StartApp(t, func(cfg *config.Config) { cfg.RabbitMQURL = deadBroker })
		cargo := c.NewCargoOnFreshVessel()
		cargoID = cargo.ID

		c.MustChangeStatus(cargo.ID, InTransit)
		c.MustChangeStatus(cargo.ID, Delivered)

		ready := c.Get("/readyz")
		assert.Equal(t, http.StatusOK, ready.Status, "the instance keeps serving traffic")
		assert.Contains(t, ready.String(), `"status":"degraded"`)
		assert.Contains(t, ready.String(), `"rabbitmq":"down"`)

		assert.Equal(t, 2, pendingOutbox(t, cargo.ID), "events wait in the outbox")
		// The relay publishes in global order and stops at the first failure,
		// so retries are recorded on the oldest pending row, whichever it is.
		assert.Eventually(t, func() bool {
			return CountRows(t, db, `SELECT COALESCE((SELECT attempts FROM outbox WHERE published_at IS NULL
				ORDER BY created_at, id LIMIT 1), 0)`) > 0
		}, 10*time.Second, 20*time.Millisecond, "failed attempts are recorded")
		assert.Empty(t, c.Events(cargo.ID))
	})

	t.Run("after the broker is back", func(t *testing.T) {
		c := StartApp(t, func(*config.Config) {})

		events := c.WaitForEvents(cargoID, 2)
		assert.Equal(t, InTransit, events[0].NewStatus, "order is preserved")
		assert.Equal(t, Delivered, events[1].NewStatus)
		assert.Zero(t, pendingOutbox(t, cargoID))
		c.AssertEventCountStays(cargoID, 2)
	})
}

func TestBrokerConnectionLostMidRun_RecoversWithoutLosingEvents(t *testing.T) {
	proxy := NewProxy(t, hostOf(t, env().RabbitMQURL))
	c := StartApp(t, func(cfg *config.Config) { cfg.RabbitMQURL = withHost(t, cfg.RabbitMQURL, proxy.Addr()) })
	cargo := c.NewCargoOnFreshVessel()
	c.MustChangeStatus(cargo.ID, InTransit)
	c.WaitForEvents(cargo.ID, 1)

	proxy.Cut()
	c.MustChangeStatus(cargo.ID, Delivered)
	assert.Eventually(t, func() bool { return strings.Contains(c.Get("/readyz").String(), "degraded") },
		10*time.Second, 50*time.Millisecond, "readiness reports the broker outage")
	assert.Equal(t, 1, pendingOutbox(t, cargo.ID))

	proxy.Restore()
	c.WaitForEvents(cargo.ID, 2)
	assert.Eventually(t, func() bool { return strings.Contains(c.Get("/readyz").String(), `"status":"ready"`) },
		15*time.Second, 50*time.Millisecond)
}

func TestDuplicateMessagesAreStoredOnce(t *testing.T) {
	c := StartApp(t, func(cfg *config.Config) { cfg.RelayEnabled = false })
	cargo := c.NewCargoOnFreshVessel()
	payload := StatusChangedPayload(t, uuid.NewString(), cargo.ID, Pending, InTransit)

	for range 5 {
		PublishRaw(t, env().RabbitMQURL, payload)
	}

	c.WaitForEvents(cargo.ID, 1)
	c.AssertEventCountStays(cargo.ID, 1)
}

func TestPoisonMessagesAreDeadLetteredAndDoNotBlockTheQueue(t *testing.T) {
	c := StartApp(t, func(*config.Config) {})
	marker := uuid.NewString()
	poison := map[string][]byte{
		"not json":       []byte(`{"broken":"` + marker),
		"unknown status": StatusChangedPayload(t, uuid.NewString(), uuid.NewString(), "lost", "found"),
		"unknown cargo":  StatusChangedPayload(t, uuid.NewString(), uuid.NewString(), Pending, InTransit),
		"missing ids":    []byte(`{"marker":"` + marker + `","old_status":"pending","new_status":"in_transit"}`),
	}
	for _, body := range poison {
		PublishRaw(t, env().RabbitMQURL, body)
	}

	for name, body := range poison {
		assert.Eventually(t, func() bool { return DeadLettered(t, env().RabbitMQURL, body) },
			15*time.Second, 100*time.Millisecond, name)
	}

	cargo := c.NewCargoOnFreshVessel()
	c.MustChangeStatus(cargo.ID, InTransit)
	c.WaitForEvents(cargo.ID, 1)
}

// Simulates the relay crashing after RabbitMQ confirmed a message but before
// the outbox row was marked published: the message is sent twice.
func TestRelayCrashAfterPublishDoesNotDuplicateEvents(t *testing.T) {
	var cargoID string

	t.Run("event committed, relay down", func(t *testing.T) {
		c := StartApp(t, func(cfg *config.Config) { cfg.RelayEnabled = false })
		cargo := c.NewCargoOnFreshVessel()
		cargoID = cargo.ID
		c.MustChangeStatus(cargo.ID, InTransit)

		var payload []byte
		require.NoError(t, db.QueryRow(context.Background(),
			`SELECT payload FROM outbox WHERE aggregate_id = $1`, cargo.ID).Scan(&payload))
		PublishRaw(t, env().RabbitMQURL, payload) // the "first" publish before the crash
		c.WaitForEvents(cargo.ID, 1)
		assert.Equal(t, 1, pendingOutbox(t, cargo.ID), "the crash left the row unmarked")
	})

	t.Run("relay restarts and republishes", func(t *testing.T) {
		c := StartApp(t, func(*config.Config) {})

		assert.Eventually(t, func() bool { return pendingOutbox(t, cargoID) == 0 }, 15*time.Second, 20*time.Millisecond)
		c.AssertEventCountStays(cargoID, 1)
	})
}

// --- database -----------------------------------------------------------

func TestDatabaseOutageMidRun_Returns503ThenRecovers(t *testing.T) {
	proxy := NewProxy(t, hostOf(t, env().DatabaseURL))
	c := StartApp(t, func(cfg *config.Config) {
		cfg.DatabaseURL = withHost(t, cfg.DatabaseURL, proxy.Addr())
		cfg.RequestTimeout = 2 * time.Second
		cfg.RelayEnabled, cfg.ConsumerEnabled = false, false
	})
	vessel := c.NewVessel(100)

	proxy.Cut()

	for _, resp := range []Response{
		c.Get(VesselPath(vessel.ID)),
		c.Get("/api/v1/cargoes"),
		c.Post("/api/v1/cargoes", map[string]any{"name": "x", "weight_kg": 1, "vessel_id": vessel.ID}),
	} {
		require.Equal(t, http.StatusServiceUnavailable, resp.Status, resp.String())
		assert.Equal(t, "5", resp.Header.Get("Retry-After"))
		assert.Equal(t, "service_unavailable", ErrorOf(t, resp).Code)
	}
	assert.Equal(t, http.StatusServiceUnavailable, c.Get("/readyz").Status)
	assert.Equal(t, http.StatusOK, c.Get("/healthz").Status, "liveness must not fail, or the orchestrator restarts a healthy process")

	proxy.Restore()

	assert.Eventually(t, func() bool { return c.Get(VesselPath(vessel.ID)).Status == http.StatusOK },
		15*time.Second, 50*time.Millisecond, "the pool reconnects by itself")
	assert.Equal(t, http.StatusOK, c.Get("/readyz").Status)
	c.NewCargo(vessel.ID, 1)
}

func TestDatabaseUnreachableAtStartup_FailsFast(t *testing.T) {
	cfg, err := AppConfig(env())
	require.NoError(t, err)
	cfg.DatabaseURL = "postgres://deus:deus@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"
	cfg.DBConnectAttempts = 3

	started := time.Now()
	_, err = app.New(context.Background(), cfg, zap.NewNop())

	assert.ErrorContains(t, err, "connect to database after 3 attempts")
	assert.Less(t, time.Since(started), 30*time.Second)
}

// A row locked by a long transaction must not hang clients: the request
// deadline turns the wait into a 503, and since nothing was committed the
// client may retry with the same Idempotency-Key.
func TestLockedRowTimesOutWith503AndKeyCanBeRetried(t *testing.T) {
	c := StartApp(t, func(cfg *config.Config) { cfg.RequestTimeout = 500 * time.Millisecond })
	cargo := c.NewCargoOnFreshVessel()
	key := IdempotencyKey(uuid.NewString())

	lock, err := db.Begin(context.Background())
	require.NoError(t, err)
	_, err = lock.Exec(context.Background(), `SELECT 1 FROM cargoes WHERE id = $1 FOR UPDATE`, cargo.ID)
	require.NoError(t, err)

	started := time.Now()
	blocked := c.ChangeStatus(cargo.ID, InTransit, key)
	elapsed := time.Since(started)
	require.NoError(t, lock.Rollback(context.Background()))

	require.Equal(t, http.StatusServiceUnavailable, blocked.Status, blocked.String())
	assert.Equal(t, "5", blocked.Header.Get("Retry-After"))
	assert.Less(t, elapsed, 3*time.Second, "the client is not held hostage by the lock")

	retry := c.ChangeStatus(cargo.ID, InTransit, key)
	assert.Equal(t, http.StatusOK, retry.Status, "5xx responses are not remembered")
	assert.Empty(t, retry.Header.Get("Idempotent-Replayed"))
	c.WaitForEvents(cargo.ID, 1)
}

func TestConsumerWaitsOutDatabaseOutageWithoutDeadLettering(t *testing.T) {
	proxy := NewProxy(t, hostOf(t, env().DatabaseURL))
	c := StartApp(t, func(cfg *config.Config) {
		cfg.DatabaseURL = withHost(t, cfg.DatabaseURL, proxy.Addr())
		cfg.RelayEnabled = false
	})
	cargo := c.NewCargoOnFreshVessel()
	payload := StatusChangedPayload(t, uuid.NewString(), cargo.ID, Pending, InTransit)

	proxy.Cut()
	PublishRaw(t, env().RabbitMQURL, payload)
	time.Sleep(time.Second) // the consumer receives it and fails to store it
	assert.False(t, DeadLettered(t, env().RabbitMQURL, payload), "a DB outage is not the message's fault")

	proxy.Restore()
	c.WaitForEvents(cargo.ID, 1)
}

// --- lifecycle and telemetry ---------------------------------------------

func TestGracefulShutdown_NoAcceptedRequestIsLost(t *testing.T) {
	running := LaunchApp(t, func(cfg *config.Config) { cfg.RelayEnabled, cfg.ConsumerEnabled = false, false })
	prefix := UniqueName("shutdown")

	var created, failedTransport atomic.Int64
	var serverErrors []string
	var mu sync.Mutex
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp, err := running.TryDo(http.MethodPost, "/api/v1/vessels",
					map[string]any{"name": prefix + "-" + uuid.NewString(), "capacity_kg": 1, "current_location": "x"})
				switch {
				case err != nil:
					failedTransport.Add(1) // refused after shutdown: never processed
				case resp.Status == http.StatusCreated:
					created.Add(1)
				default:
					mu.Lock()
					serverErrors = append(serverErrors, resp.String())
					mu.Unlock()
				}
			}
		}()
	}

	time.Sleep(300 * time.Millisecond)
	require.NoError(t, running.Stop(), "shutdown completes cleanly")
	close(stop)
	wg.Wait()

	assert.Empty(t, serverErrors, "in-flight requests finish normally")
	assert.Positive(t, created.Load())
	stored := CountRows(t, db, `SELECT COUNT(*) FROM vessels WHERE name LIKE $1`, prefix+"-%")
	assert.Equal(t, int(created.Load()), stored, "every 201 was persisted and nothing else was")
}

func TestTelemetryCollectorDown_RequestsAreUnaffectedAndTracesPropagate(t *testing.T) {
	c := StartApp(t, func(cfg *config.Config) { cfg.OTelEndpoint = "http://127.0.0.1:1" })
	deliveries := TapEvents(t, env().RabbitMQURL)
	cargo := c.NewCargoOnFreshVessel()

	started := time.Now()
	c.MustChangeStatus(cargo.ID, InTransit)
	assert.Less(t, time.Since(started), 2*time.Second)

	timeout := time.After(15 * time.Second)
	for {
		select {
		case d := <-deliveries:
			if !strings.Contains(string(d.Body), cargo.ID) {
				continue
			}
			traceparent, _ := d.Headers["traceparent"].(string)
			assert.Regexp(t, `^00-[0-9a-f]{32}-[0-9a-f]{16}-0[01]$`, traceparent, "W3C trace context travels with the message")
			c.WaitForEvents(cargo.ID, 1)
			return
		case <-timeout:
			t.Fatal("event was not published")
		}
	}
}
