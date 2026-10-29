// Package app is the composition root: it builds the object graph from
// configuration and owns the process lifecycle.
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.uber.org/zap"

	"github.com/drzbraz/deus.ai/internal/backoff"
	"github.com/drzbraz/deus.ai/internal/config"
	"github.com/drzbraz/deus.ai/internal/events"
	"github.com/drzbraz/deus.ai/internal/events/rabbitmq"
	"github.com/drzbraz/deus.ai/internal/idempotency"
	"github.com/drzbraz/deus.ai/internal/repository/postgres"
	"github.com/drzbraz/deus.ai/internal/service"
	"github.com/drzbraz/deus.ai/internal/telemetry"
	"github.com/drzbraz/deus.ai/internal/transport/httpapi"
)

const idempotencyCleanupInterval = time.Hour

// worker is a background loop that runs until its context is cancelled.
type worker struct {
	name string
	run  func(ctx context.Context)
}

// App is a fully wired, listening service.
type App struct {
	cfg               config.Config
	log               *zap.Logger
	pool              *pgxpool.Pool
	publisher         *rabbitmq.Publisher
	server            *http.Server
	listener          net.Listener
	workers           []worker
	shutdownTelemetry telemetry.Shutdown
}

// New connects to every dependency, applies migrations and starts listening.
// Nothing is served until Run is called.
func New(ctx context.Context, cfg config.Config, log *zap.Logger) (app *App, err error) {
	a := &App{cfg: cfg, log: log}
	defer func() {
		if err != nil {
			a.release(context.WithoutCancel(ctx))
		}
	}()

	if a.shutdownTelemetry, err = telemetry.Setup(ctx, telemetry.Config{
		Endpoint: cfg.OTelEndpoint, ServiceName: cfg.OTelServiceName,
	}, log); err != nil {
		return nil, fmt.Errorf("setup telemetry: %w", err)
	}

	if a.pool, err = postgres.Connect(ctx, postgres.PoolConfig{
		URL:              cfg.DatabaseURL,
		MaxConns:         cfg.DBMaxConns,
		StatementTimeout: cfg.DBStatementTimeout,
		ConnectAttempts:  cfg.DBConnectAttempts,
	}, backoff.Default(), log); err != nil {
		return nil, err
	}
	if err = postgres.Migrate(cfg.DatabaseURL); err != nil {
		return nil, err
	}

	a.publisher = rabbitmq.NewPublisher(cfg.RabbitMQURL)
	if err = a.wire(); err != nil {
		return nil, err
	}

	var listenConfig net.ListenConfig
	if a.listener, err = listenConfig.Listen(ctx, "tcp", cfg.HTTPAddr); err != nil {
		return nil, fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
	}
	return a, nil
}

// wire builds services, the HTTP router and the background workers.
func (a *App) wire() error {
	meter := otel.Meter(telemetry.MeterName)
	metrics, err := telemetry.NewMetrics(meter)
	if err != nil {
		return fmt.Errorf("create metrics: %w", err)
	}

	repos := postgres.NewRepositories(a.pool)
	outbox := postgres.NewOutboxRepository(a.pool)
	idempotencyStore := postgres.NewIdempotencyRepository(a.pool)
	if err := telemetry.RegisterOutboxBacklog(meter, outbox.CountPending); err != nil {
		return fmt.Errorf("register outbox gauge: %w", err)
	}

	router := httpapi.NewRouter(httpapi.Dependencies{
		Vessels:          service.NewVesselService(repos, service.SystemClock),
		Cargoes:          service.NewCargoService(repos, service.SystemClock, metrics),
		Tracking:         service.NewTrackingService(repos, service.SystemClock),
		Tx:               repos.Tx,
		IdempotencyStore: idempotencyStore,
		HealthChecks: []httpapi.HealthCheck{
			{Name: "postgres", Critical: true, Check: a.pool.Ping},
			{Name: "rabbitmq", Critical: false, Check: a.publisher.Check},
		},
		Logger:         a.log,
		RequestTimeout: a.cfg.RequestTimeout,
	})
	a.server = &http.Server{
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      a.cfg.RequestTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
	}

	if a.cfg.RelayEnabled {
		relay := events.NewRelay(repos.Tx, outbox, a.publisher, metrics, a.log,
			events.WithInterval(a.cfg.OutboxInterval), events.WithBatchSize(a.cfg.OutboxBatchSize))
		a.workers = append(a.workers, worker{"outbox relay", relay.Run})
	}
	if a.cfg.ConsumerEnabled {
		handler := events.NewHandler(postgres.NewCargoEventRepository(a.pool), metrics, a.log, backoff.Default())
		consumer := rabbitmq.NewConsumer(a.cfg.RabbitMQURL, handler, a.log, backoff.Default())
		a.workers = append(a.workers, worker{"event consumer", consumer.Run})
	}
	a.workers = append(a.workers, worker{"idempotency cleanup", func(ctx context.Context) {
		every(ctx, idempotencyCleanupInterval, func(ctx context.Context) {
			a.deleteExpiredIdempotencyKeys(ctx, idempotencyStore)
		})
	}})
	return nil
}

// Addr is the address the HTTP server listens on.
func (a *App) Addr() string {
	return a.listener.Addr().String()
}

// Run serves traffic and runs the background workers until ctx is
// cancelled, then shuts down gracefully:
//  1. stop accepting connections and drain in-flight requests,
//  2. stop the workers (the consumer requeues what it has not finished),
//  3. flush telemetry and close connections.
func (a *App) Run(ctx context.Context) error {
	workersCtx, stopWorkers := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	for _, w := range a.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.log.Info("worker started", zap.String("worker", w.name))
			w.run(workersCtx)
			a.log.Info("worker stopped", zap.String("worker", w.name))
		}()
	}

	a.log.Info("http server listening", zap.String("addr", a.Addr()))
	serveErr := serve(ctx, a.server, a.listener, a.cfg.ShutdownTimeout)

	stopWorkers()
	wg.Wait()
	a.release(context.WithoutCancel(ctx))
	a.log.Info("shutdown complete")
	return serveErr
}

// serve runs srv until ctx is cancelled or the server fails, then drains
// in-flight requests for at most shutdownTimeout.
func serve(ctx context.Context, srv *http.Server, ln net.Listener, shutdownTimeout time.Duration) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("drain http server: %w", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

func (a *App) release(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.ShutdownTimeout)
	defer cancel()

	if a.listener != nil {
		_ = a.listener.Close()
	}
	if a.publisher != nil {
		_ = a.publisher.Close()
	}
	if a.pool != nil {
		a.pool.Close()
	}
	if a.shutdownTelemetry != nil {
		if err := a.shutdownTelemetry(ctx); err != nil {
			a.log.Warn("flush telemetry", zap.Error(err))
		}
	}
}

func (a *App) deleteExpiredIdempotencyKeys(ctx context.Context, store idempotency.Store) {
	deleted, err := store.DeleteExpired(ctx, time.Now().Add(-a.cfg.IdempotencyTTL))
	if err != nil {
		a.log.Warn("delete expired idempotency keys", zap.Error(err))
		return
	}
	a.log.Debug("expired idempotency keys deleted", zap.Int64("count", deleted))
}

// every runs fn immediately and then on each tick until ctx is cancelled.
func every(ctx context.Context, interval time.Duration, fn func(ctx context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		fn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
