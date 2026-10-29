//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/drzbraz/deus.ai/internal/backoff"
	"github.com/drzbraz/deus.ai/internal/domain"
	"github.com/drzbraz/deus.ai/internal/repository/postgres"
	"github.com/drzbraz/deus.ai/internal/service"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://deus:deus@localhost:55432/logistics_test?sslmode=disable"
	}

	pool, err := postgres.Connect(context.Background(), postgres.PoolConfig{
		URL: url, MaxConns: 30, StatementTimeout: 5 * time.Second, ConnectAttempts: 20,
	}, backoff.Default(), zap.NewNop())
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration database unavailable:", err)
		os.Exit(1)
	}
	if err := postgres.Migrate(url); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
	testPool = pool

	code := m.Run()
	pool.Close()
	os.Exit(code)
}

func repos() service.Repositories {
	return postgres.NewRepositories(testPool)
}

// newVessel creates a vessel with a unique name; tests never truncate tables
// (the audit tables forbid it), they isolate themselves with fresh data.
func newVessel(t *testing.T, capacityKg float64) domain.Vessel {
	t.Helper()
	vessel, err := domain.NewVessel("vessel-"+uuid.NewString(), capacityKg, "Rotterdam", service.SystemClock())
	require.NoError(t, err)
	require.NoError(t, postgres.NewVesselRepository(testPool).Create(context.Background(), vessel))
	return vessel
}

func newCargo(t *testing.T, vesselID uuid.UUID, weightKg float64) domain.Cargo {
	t.Helper()
	cargo, err := domain.NewCargo(domain.NewCargoParams{Name: "cargo", WeightKg: weightKg, VesselID: vesselID}, service.SystemClock())
	require.NoError(t, err)
	require.NoError(t, postgres.NewCargoRepository(testPool).Create(context.Background(), cargo))
	return cargo
}
