package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5:// driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/drzbraz/deus.ai/internal/backoff"
	"github.com/drzbraz/deus.ai/migrations"
)

// PoolConfig controls the connection pool and how hard we try to connect.
type PoolConfig struct {
	URL              string
	MaxConns         int32
	StatementTimeout time.Duration
	ConnectAttempts  int
}

// Connect opens a pool and waits for the database with exponential backoff,
// so the API survives starting before Postgres is ready.
func Connect(ctx context.Context, cfg PoolConfig, policy backoff.Policy, log *zap.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	poolCfg.MaxConns = cfg.MaxConns
	// A server-side limit protects the database from runaway queries even if
	// a caller forgets to set a context deadline.
	poolCfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(cfg.StatementTimeout.Milliseconds(), 10)
	poolCfg.ConnConfig.Tracer = otelpgx.NewTracer()

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	attempt := 0
	err = backoff.Retry(ctx, policy, cfg.ConnectAttempts, func(ctx context.Context) error {
		attempt++
		pingErr := pool.Ping(ctx)
		if pingErr != nil {
			log.Warn("database not ready, retrying", zap.Int("attempt", attempt), zap.Error(pingErr))
		}
		return pingErr
	})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database after %d attempts: %w", attempt, err)
	}
	return pool, nil
}

// Migrate applies every pending migration. golang-migrate holds a Postgres
// advisory lock while running, so concurrent replicas cannot migrate twice.
func Migrate(databaseURL string) error {
	source, err := iofs.New(migrations.Files, ".")
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, toMigrateURL(databaseURL))
	if err != nil {
		return fmt.Errorf("init migrations: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

func toMigrateURL(databaseURL string) string {
	for _, scheme := range []string{"postgresql://", "postgres://"} {
		if rest, ok := strings.CutPrefix(databaseURL, scheme); ok {
			return "pgx5://" + rest
		}
	}
	return databaseURL
}
