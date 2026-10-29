//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/drzbraz/deus.ai/internal/backoff"
	"github.com/drzbraz/deus.ai/internal/domain"
	"github.com/drzbraz/deus.ai/internal/repository/postgres"
	"github.com/drzbraz/deus.ai/internal/service"
)

func TestVesselRepository_CRUD(t *testing.T) {
	ctx := context.Background()
	repo := postgres.NewVesselRepository(testPool)
	vessel := newVessel(t, 1000)

	got, err := repo.Get(ctx, vessel.ID)
	require.NoError(t, err)
	assert.Equal(t, vessel.Name, got.Name)
	assert.Equal(t, 1000.0, got.CapacityKg)
	assert.Zero(t, got.LoadKg)
	assert.Equal(t, vessel.CreatedAt, got.CreatedAt)

	require.NoError(t, vessel.MoveTo("Hamburg", service.SystemClock()))
	require.NoError(t, repo.Update(ctx, vessel))
	got, err = repo.Get(ctx, vessel.ID)
	require.NoError(t, err)
	assert.Equal(t, "Hamburg", got.CurrentLocation)

	list, err := repo.List(ctx, domain.Page{Limit: 100})
	require.NoError(t, err)
	assert.NotEmpty(t, list)
}

func TestVesselRepository_Errors(t *testing.T) {
	ctx := context.Background()
	repo := postgres.NewVesselRepository(testPool)
	vessel := newVessel(t, 10)

	duplicate := vessel
	duplicate.ID = uuid.New()
	assert.ErrorIs(t, repo.Create(ctx, duplicate), domain.ErrAlreadyExists)

	_, err := repo.Get(ctx, uuid.New())
	assert.ErrorIs(t, err, domain.ErrNotFound)

	_, err = repo.GetForUpdate(ctx, uuid.New())
	assert.ErrorIs(t, err, domain.ErrNotFound)

	missing := vessel
	missing.ID = uuid.New()
	assert.ErrorIs(t, repo.Update(ctx, missing), domain.ErrNotFound)
}

func TestVesselRepository_LoadIgnoresDeliveredCargo(t *testing.T) {
	ctx := context.Background()
	vessel := newVessel(t, 1000)
	newCargo(t, vessel.ID, 100)
	delivered := newCargo(t, vessel.ID, 250)
	delivered.Status = domain.StatusDelivered
	require.NoError(t, postgres.NewCargoRepository(testPool).Update(ctx, delivered))

	got, err := postgres.NewVesselRepository(testPool).Get(ctx, vessel.ID)

	require.NoError(t, err)
	assert.Equal(t, 100.0, got.LoadKg)
}

func TestCargoRepository_CRUDAndFilters(t *testing.T) {
	ctx := context.Background()
	repo := postgres.NewCargoRepository(testPool)
	vessel := newVessel(t, 1000)
	pending := newCargo(t, vessel.ID, 10)
	moving := newCargo(t, vessel.ID, 20)
	_, err := moving.ChangeStatus(domain.StatusInTransit, service.SystemClock())
	require.NoError(t, err)
	require.NoError(t, repo.Update(ctx, moving))

	got, err := repo.Get(ctx, moving.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusInTransit, got.Status)
	assert.Equal(t, 2, got.Version)

	all, err := repo.List(ctx, domain.CargoFilter{VesselID: &vessel.ID, Page: domain.Page{Limit: 10}})
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{pending.ID, moving.ID}, ids(all))

	status := domain.StatusInTransit
	filtered, err := repo.List(ctx, domain.CargoFilter{VesselID: &vessel.ID, Status: &status, Page: domain.Page{Limit: 10}})
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{moving.ID}, ids(filtered))

	paged, err := repo.List(ctx, domain.CargoFilter{VesselID: &vessel.ID, Page: domain.Page{Limit: 1, Offset: 1}})
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{moving.ID}, ids(paged))
}

func TestCargoRepository_Errors(t *testing.T) {
	ctx := context.Background()
	repo := postgres.NewCargoRepository(testPool)

	orphan, err := domain.NewCargo(domain.NewCargoParams{Name: "x", WeightKg: 1, VesselID: uuid.New()}, service.SystemClock())
	require.NoError(t, err)
	assert.ErrorIs(t, repo.Create(ctx, orphan), domain.ErrNotFound, "foreign key violation")

	_, err = repo.Get(ctx, uuid.New())
	assert.ErrorIs(t, err, domain.ErrNotFound)

	_, err = repo.GetForUpdate(ctx, uuid.New())
	assert.ErrorIs(t, err, domain.ErrNotFound)

	assert.ErrorIs(t, repo.Update(ctx, orphan), domain.ErrNotFound)
}

func TestTrackingRepository_AppendAndListInOrder(t *testing.T) {
	ctx := context.Background()
	repo := postgres.NewTrackingRepository(testPool)
	cargo := newCargo(t, newVessel(t, 100).ID, 1)
	start := service.SystemClock()

	for i, location := range []string{"Lisbon", "Gibraltar", "Suez"} {
		entry, err := domain.NewTrackingEntry(cargo, location, "", start.Add(time.Duration(i)*time.Minute))
		require.NoError(t, err)
		require.NoError(t, repo.Append(ctx, entry))
	}

	entries, err := repo.ListByCargo(ctx, cargo.ID)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	assert.Equal(t, "Lisbon", entries[0].Location)
	assert.Equal(t, "Suez", entries[2].Location)
	assert.Equal(t, domain.StatusPending, entries[0].Status)

	empty, err := repo.ListByCargo(ctx, uuid.New())
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestAuditTables_AreAppendOnly(t *testing.T) {
	ctx := context.Background()
	cargo := newCargo(t, newVessel(t, 100).ID, 1)
	entry, err := domain.NewTrackingEntry(cargo, "Lisbon", "", service.SystemClock())
	require.NoError(t, err)
	require.NoError(t, postgres.NewTrackingRepository(testPool).Append(ctx, entry))
	_, err = postgres.NewCargoEventRepository(testPool).Save(ctx, statusChanged(cargo.ID))
	require.NoError(t, err)

	forbidden := []string{
		`UPDATE tracking_entries SET location = 'tampered' WHERE cargo_id = $1`,
		`DELETE FROM tracking_entries WHERE cargo_id = $1`,
		`UPDATE cargo_events SET new_status = 'pending' WHERE cargo_id = $1`,
		`DELETE FROM cargo_events WHERE cargo_id = $1`,
	}
	for _, sql := range forbidden {
		_, err := testPool.Exec(ctx, sql, cargo.ID)
		assert.ErrorContains(t, err, "append-only", sql)
	}

	for _, table := range []string{"tracking_entries", "cargo_events"} {
		_, err := testPool.Exec(ctx, `TRUNCATE `+table+` CASCADE`)
		assert.ErrorContains(t, err, "append-only", table)
	}
}

func TestCargoEventRepository_SaveIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo := postgres.NewCargoEventRepository(testPool)
	cargo := newCargo(t, newVessel(t, 100).ID, 1)
	event := statusChanged(cargo.ID)

	inserted, err := repo.Save(ctx, event)
	require.NoError(t, err)
	assert.True(t, inserted)

	inserted, err = repo.Save(ctx, event)
	require.NoError(t, err)
	assert.False(t, inserted, "redelivery must be ignored")

	stored, err := repo.ListByCargo(ctx, cargo.ID)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.Equal(t, event.EventID, stored[0].EventID)
	assert.Equal(t, domain.StatusInTransit, stored[0].NewStatus)
	assert.False(t, stored[0].ReceivedAt.IsZero())
}

func TestCargoEventRepository_UnknownCargoIsNotFound(t *testing.T) {
	_, err := postgres.NewCargoEventRepository(testPool).Save(context.Background(), statusChanged(uuid.New()))

	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestTxManager_RollsBackOnError(t *testing.T) {
	ctx := context.Background()
	tx := postgres.NewTxManager(testPool)
	vessels := postgres.NewVesselRepository(testPool)
	vessel, err := domain.NewVessel("rollback-"+uuid.NewString(), 10, "Lisbon", service.SystemClock())
	require.NoError(t, err)
	boom := errors.New("boom")

	err = tx.WithinTx(ctx, func(ctx context.Context) error {
		require.NoError(t, vessels.Create(ctx, vessel))
		return boom
	})

	assert.ErrorIs(t, err, boom)
	_, err = vessels.Get(ctx, vessel.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestTxManager_NestedFailureOnlyRollsBackSavepoint(t *testing.T) {
	ctx := context.Background()
	tx := postgres.NewTxManager(testPool)
	vessels := postgres.NewVesselRepository(testPool)
	outer, _ := domain.NewVessel("outer-"+uuid.NewString(), 10, "Lisbon", service.SystemClock())
	inner, _ := domain.NewVessel("inner-"+uuid.NewString(), 10, "Lisbon", service.SystemClock())

	err := tx.WithinTx(ctx, func(ctx context.Context) error {
		require.NoError(t, vessels.Create(ctx, outer))
		innerErr := tx.WithinTx(ctx, func(ctx context.Context) error {
			require.NoError(t, vessels.Create(ctx, inner))
			return errors.New("inner failed")
		})
		assert.Error(t, innerErr)
		return nil
	})

	require.NoError(t, err)
	_, err = vessels.Get(ctx, outer.ID)
	assert.NoError(t, err, "outer write is committed")
	_, err = vessels.Get(ctx, inner.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound, "inner write is rolled back to the savepoint")
}

func TestConnect_GivesUpWhenDatabaseIsUnreachable(t *testing.T) {
	_, err := postgres.Connect(context.Background(), postgres.PoolConfig{
		URL: "postgres://deus:deus@127.0.0.1:1/none?connect_timeout=1", MaxConns: 1,
		StatementTimeout: time.Second, ConnectAttempts: 2,
	}, backoff.None{}, zap.NewNop())

	assert.ErrorContains(t, err, "after 2 attempts")
}

func TestConnect_RejectsInvalidURL(t *testing.T) {
	_, err := postgres.Connect(context.Background(), postgres.PoolConfig{URL: "::not a url::"}, backoff.None{}, zap.NewNop())
	assert.Error(t, err)
	assert.Error(t, postgres.Migrate("postgres://deus:deus@127.0.0.1:1/none?connect_timeout=1"))
}

func TestDatabaseErrorsMapToUnavailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)

	_, err := postgres.NewVesselRepository(testPool).Get(ctx, uuid.New())

	assert.ErrorIs(t, err, domain.ErrUnavailable)
}

func statusChanged(cargoID uuid.UUID) domain.StatusChanged {
	return domain.StatusChanged{
		EventID: uuid.New(), CargoID: cargoID,
		OldStatus: domain.StatusPending, NewStatus: domain.StatusInTransit,
		OccurredAt: service.SystemClock(),
	}
}

func ids(cargoes []domain.Cargo) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(cargoes))
	for _, c := range cargoes {
		out = append(out, c.ID)
	}
	return out
}
