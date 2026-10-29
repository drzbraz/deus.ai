// Package testkit is the harness of the black-box test suites under tests/.
// Suites talk to the API only over HTTP and verify side effects directly in
// Postgres and RabbitMQ, so they test the system the way a client sees it.
//
// By default the API is started in-process against the disposable test
// infrastructure (docker-compose.test.yml). Set API_BASE_URL to run the
// suites against an already running deployment (e.g. `make docker-run`).
package testkit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/drzbraz/deus.ai/internal/app"
	"github.com/drzbraz/deus.ai/internal/config"
)

const (
	defaultDatabaseURL = "postgres://deus:deus@localhost:55432/logistics_test?sslmode=disable"
	defaultRabbitMQURL = "amqp://deus_user:deus_pass@localhost:56720/"
)

// Env says where the system under test and its dependencies live.
type Env struct {
	BaseURL     string // empty: start the API in-process
	DatabaseURL string
	RabbitMQURL string
}

func LoadEnv() Env {
	return Env{
		BaseURL:     os.Getenv("API_BASE_URL"),
		DatabaseURL: firstNonEmpty(os.Getenv("E2E_DATABASE_URL"), os.Getenv("TEST_DATABASE_URL"), defaultDatabaseURL),
		RabbitMQURL: firstNonEmpty(os.Getenv("E2E_RABBITMQ_URL"), os.Getenv("TEST_RABBITMQ_URL"), defaultRabbitMQURL),
	}
}

// External reports whether the suite targets a deployment it does not control.
func (e Env) External() bool { return e.BaseURL != "" }

// Stack is a running API plus a database handle for verification.
type Stack struct {
	Env     Env
	BaseURL string
	DB      *pgxpool.Pool
	stop    func()
}

// Main boots a Stack shared by every test of a package, runs the tests and
// tears it down. Use it from TestMain.
func Main(m *testing.M, stack **Stack) int {
	s, err := start(context.Background(), LoadEnv())
	if err != nil {
		fmt.Fprintln(os.Stderr, "test stack:", err)
		return 1
	}
	defer s.close()

	*stack = s
	return m.Run()
}

func start(ctx context.Context, env Env) (*Stack, error) {
	db, err := connectDB(ctx, env.DatabaseURL)
	if err != nil {
		return nil, err
	}
	s := &Stack{Env: env, BaseURL: env.BaseURL, DB: db, stop: func() {}}
	if env.External() {
		return s, nil
	}

	cfg, err := AppConfig(env)
	if err != nil {
		db.Close()
		return nil, err
	}
	baseURL, stop, err := runApp(ctx, cfg, zap.NewNop())
	if err != nil {
		db.Close()
		return nil, err
	}
	s.BaseURL, s.stop = baseURL, stop
	return s, nil
}

func (s *Stack) close() {
	s.stop()
	s.DB.Close()
}

// Client returns an HTTP client bound to the stack's API.
func (s *Stack) Client(t testing.TB) *Client {
	return NewClient(t, s.BaseURL)
}

// AppConfig points an in-process API at the test infrastructure, with short
// intervals so asynchronous assertions settle quickly.
func AppConfig(env Env) (config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, err
	}
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.DatabaseURL = env.DatabaseURL
	cfg.RabbitMQURL = env.RabbitMQURL
	cfg.DBMaxConns = 40
	cfg.OutboxInterval = 20 * time.Millisecond
	cfg.ShutdownTimeout = 5 * time.Second
	cfg.OTelEndpoint = ""
	return cfg, nil
}

// StartApp boots an extra in-process API with a customised configuration,
// for tests that need to control dependencies. It stops when the test ends.
func StartApp(t testing.TB, customize func(cfg *config.Config)) *Client {
	t.Helper()
	cfg, err := AppConfig(LoadEnv())
	if err != nil {
		t.Fatalf("app config: %v", err)
	}
	customize(&cfg)

	baseURL, stop, err := runApp(context.Background(), cfg, zaptest.NewLogger(t, zaptest.Level(zap.ErrorLevel)))
	if err != nil {
		t.Fatalf("start app: %v", err)
	}
	t.Cleanup(stop)
	return NewClient(t, baseURL)
}

// RunningApp gives a test control over the lifecycle of an in-process API.
type RunningApp struct {
	*Client
	cancel context.CancelFunc
	done   chan error
}

// LaunchApp starts an API whose shutdown the test triggers with Stop.
func LaunchApp(t testing.TB, customize func(cfg *config.Config)) *RunningApp {
	t.Helper()
	cfg, err := AppConfig(LoadEnv())
	if err != nil {
		t.Fatalf("app config: %v", err)
	}
	customize(&cfg)

	ctx, cancel := context.WithCancel(context.Background())
	application, err := app.New(ctx, cfg, zaptest.NewLogger(t, zaptest.Level(zap.ErrorLevel)))
	if err != nil {
		cancel()
		t.Fatalf("start app: %v", err)
	}

	running := &RunningApp{Client: NewClient(t, "http://"+application.Addr()), cancel: cancel, done: make(chan error, 1)}
	go func() { running.done <- application.Run(ctx) }()
	t.Cleanup(func() { _ = running.Stop() })
	return running
}

// Stop triggers a graceful shutdown (like SIGTERM) and waits for it.
func (r *RunningApp) Stop() error {
	r.cancel()
	select {
	case err, ok := <-r.done:
		if ok {
			close(r.done)
			return err
		}
		return nil
	case <-time.After(30 * time.Second):
		return errors.New("app did not stop within 30s")
	}
}

func runApp(ctx context.Context, cfg config.Config, log *zap.Logger) (baseURL string, stop func(), err error) {
	ctx, cancel := context.WithCancel(ctx)
	application, err := app.New(ctx, cfg, log)
	if err != nil {
		cancel()
		return "", nil, err
	}

	done := make(chan error, 1)
	go func() { done <- application.Run(ctx) }()
	return "http://" + application.Addr(), func() { cancel(); <-done }, nil
}

func connectDB(ctx context.Context, url string) (*pgxpool.Pool, error) {
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect verification database: %w", err)
	}
	if err := db.Ping(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping verification database %s: %w", url, err)
	}
	return db, nil
}

// SkipIfExternal skips tests that must control the API's dependencies.
func SkipIfExternal(t testing.TB) {
	t.Helper()
	if LoadEnv().External() {
		t.Skip("needs an in-process API (API_BASE_URL is set)")
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
