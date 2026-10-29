//go:build e2e

// Package schema verifies the database itself: migrations apply and roll
// back cleanly, and integrity rules hold even for writes that bypass the API
// (a buggy release, a manual fix, another service).
package schema

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drzbraz/deus.ai/migrations"
	"github.com/drzbraz/deus.ai/tests/testkit"
)

// db is a freshly created database, migrated once for the whole package.
var (
	db    *pgxpool.Pool
	dbURL string
)

func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "schema suite:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	ctx := context.Background()
	adminURL := testkit.LoadEnv().DatabaseURL
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		return 0, err
	}
	defer admin.Close()

	name := "schema_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return 0, fmt.Errorf("create database: %w", err)
	}
	defer func() { _, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") }()

	u, _ := url.Parse(adminURL)
	u.Path = "/" + name
	dbURL = u.String()

	if err := newMigrator(dbURL).Up(); err != nil {
		return 0, fmt.Errorf("migrate up: %w", err)
	}
	if db, err = pgxpool.New(ctx, dbURL); err != nil {
		return 0, err
	}
	defer db.Close()
	return m.Run(), nil
}

func newMigrator(databaseURL string) *migrate.Migrate {
	source, err := iofs.New(migrations.Files, ".")
	if err != nil {
		panic(err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", source, strings.Replace(databaseURL, "postgres://", "pgx5://", 1))
	if err != nil {
		panic(err)
	}
	return m
}

// sqlState runs a statement and returns the Postgres error code ("" = success).
func sqlState(t *testing.T, sql string, args ...any) string {
	t.Helper()
	_, err := db.Exec(context.Background(), sql, args...)
	if err == nil {
		return ""
	}
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "unexpected error: %v", err)
	return pgErr.Code
}

const (
	checkViolation      = "23514"
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
	notNullViolation    = "23502"
	restrictViolation   = "23001"
	invalidEnumValue    = "22P02"
)

func insertVessel(t *testing.T) string {
	t.Helper()
	id := uuid.NewString()
	require.Empty(t, sqlState(t, `INSERT INTO vessels (id, name, capacity_kg, current_location) VALUES ($1, $2, 100, 'Lisbon')`, id, "v-"+id))
	return id
}

func insertCargo(t *testing.T, vesselID string) string {
	t.Helper()
	id := uuid.NewString()
	require.Empty(t, sqlState(t, `INSERT INTO cargoes (id, name, weight_kg, vessel_id) VALUES ($1, 'c', 1, $2)`, id, vesselID))
	return id
}

func TestMigrations_RoundTripDownAndUp(t *testing.T) {
	ctx := context.Background()
	m := newMigrator(dbURL)
	defer m.Close()

	version, dirty, err := m.Version()
	require.NoError(t, err)
	assert.False(t, dirty)
	assert.EqualValues(t, 7, version)

	require.NoError(t, m.Down(), "every down migration runs cleanly")
	var tables int
	require.NoError(t, db.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name <> 'schema_migrations'`).Scan(&tables))
	assert.Zero(t, tables, "down removes everything it created")

	require.NoError(t, m.Up(), "migrations re-apply on a database that was rolled back")
	assert.ErrorIs(t, m.Up(), migrate.ErrNoChange, "up is idempotent")
}

func TestSchema_ExpectedIndexesExist(t *testing.T) {
	rows, err := db.Query(context.Background(), `SELECT indexname FROM pg_indexes WHERE schemaname = 'public'`)
	require.NoError(t, err)
	var indexes []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		indexes = append(indexes, name)
	}

	for _, want := range []string{
		"vessels_name_key",
		"cargoes_vessel_id_status_idx",
		"cargoes_status_idx",
		"cargoes_created_at_idx",
		"tracking_entries_cargo_id_recorded_at_idx",
		"outbox_unpublished_idx",
		"cargo_events_cargo_id_occurred_at_idx",
		"idempotency_keys_pkey",
		"idempotency_keys_created_at_idx",
	} {
		assert.Contains(t, indexes, want)
	}
}

func TestSchema_VesselConstraints(t *testing.T) {
	existing := insertVessel(t)
	var name string
	require.NoError(t, db.QueryRow(context.Background(), `SELECT name FROM vessels WHERE id = $1`, existing).Scan(&name))

	tests := map[string]struct {
		sql  string
		args []any
		want string
	}{
		"zero capacity":     {`INSERT INTO vessels (id, name, capacity_kg, current_location) VALUES ($1, 'a', 0, 'x')`, []any{uuid.New()}, checkViolation},
		"negative capacity": {`INSERT INTO vessels (id, name, capacity_kg, current_location) VALUES ($1, 'b', -1, 'x')`, []any{uuid.New()}, checkViolation},
		"empty name":        {`INSERT INTO vessels (id, name, capacity_kg, current_location) VALUES ($1, '', 1, 'x')`, []any{uuid.New()}, checkViolation},
		"empty location":    {`INSERT INTO vessels (id, name, capacity_kg, current_location) VALUES ($1, 'c', 1, '')`, []any{uuid.New()}, checkViolation},
		"null name":         {`INSERT INTO vessels (id, name, capacity_kg, current_location) VALUES ($1, NULL, 1, 'x')`, []any{uuid.New()}, notNullViolation},
		"duplicate name":    {`INSERT INTO vessels (id, name, capacity_kg, current_location) VALUES ($1, $2, 1, 'x')`, []any{uuid.New(), name}, uniqueViolation},
		"duplicate id":      {`INSERT INTO vessels (id, name, capacity_kg, current_location) VALUES ($1, 'd', 1, 'x')`, []any{existing}, uniqueViolation},
	}
	for label, tt := range tests {
		t.Run(label, func(t *testing.T) {
			assert.Equal(t, tt.want, sqlState(t, tt.sql, tt.args...))
		})
	}
}

func TestSchema_CargoConstraints(t *testing.T) {
	vesselID := insertVessel(t)

	tests := map[string]struct {
		sql  string
		args []any
		want string
	}{
		"unknown vessel": {`INSERT INTO cargoes (id, name, weight_kg, vessel_id) VALUES ($1, 'c', 1, $2)`, []any{uuid.New(), uuid.New()}, foreignKeyViolation},
		"zero weight":    {`INSERT INTO cargoes (id, name, weight_kg, vessel_id) VALUES ($1, 'c', 0, $2)`, []any{uuid.New(), vesselID}, checkViolation},
		"empty name":     {`INSERT INTO cargoes (id, name, weight_kg, vessel_id) VALUES ($1, '', 1, $2)`, []any{uuid.New(), vesselID}, checkViolation},
		"unknown status": {`INSERT INTO cargoes (id, name, weight_kg, vessel_id, status) VALUES ($1, 'c', 1, $2, 'lost')`, []any{uuid.New(), vesselID}, invalidEnumValue},
		"version zero":   {`INSERT INTO cargoes (id, name, weight_kg, vessel_id, version) VALUES ($1, 'c', 1, $2, 0)`, []any{uuid.New(), vesselID}, checkViolation},
		"missing vessel": {`INSERT INTO cargoes (id, name, weight_kg) VALUES ($1, 'c', 1)`, []any{uuid.New()}, notNullViolation},
	}
	for label, tt := range tests {
		t.Run(label, func(t *testing.T) {
			assert.Equal(t, tt.want, sqlState(t, tt.sql, tt.args...))
		})
	}
}

func TestSchema_DefaultsForNewCargo(t *testing.T) {
	cargoID := insertCargo(t, insertVessel(t))

	var status string
	var version int
	require.NoError(t, db.QueryRow(context.Background(), `SELECT status::text, version FROM cargoes WHERE id = $1`, cargoID).Scan(&status, &version))
	assert.Equal(t, "pending", status)
	assert.Equal(t, 1, version)
}

func TestSchema_ReferencedRowsCannotBeDeleted(t *testing.T) {
	vesselID := insertVessel(t)
	cargoID := insertCargo(t, vesselID)
	require.Empty(t, sqlState(t, `INSERT INTO tracking_entries (id, cargo_id, status, location) VALUES ($1, $2, 'pending', 'x')`, uuid.New(), cargoID))

	assert.Equal(t, foreignKeyViolation, sqlState(t, `DELETE FROM vessels WHERE id = $1`, vesselID), "a vessel with cargo cannot disappear")
	assert.Equal(t, foreignKeyViolation, sqlState(t, `DELETE FROM cargoes WHERE id = $1`, cargoID), "a cargo with history cannot disappear")
}

func TestSchema_AuditTablesAreAppendOnly(t *testing.T) {
	cargoID := insertCargo(t, insertVessel(t))
	require.Empty(t, sqlState(t, `INSERT INTO tracking_entries (id, cargo_id, status, location) VALUES ($1, $2, 'pending', 'x')`, uuid.New(), cargoID))
	require.Empty(t, sqlState(t, `INSERT INTO cargo_events (id, cargo_id, old_status, new_status, occurred_at) VALUES ($1, $2, 'pending', 'in_transit', now())`, uuid.New(), cargoID))

	for _, sql := range []string{
		`UPDATE tracking_entries SET location = 'forged' WHERE cargo_id = $1`,
		`DELETE FROM tracking_entries WHERE cargo_id = $1`,
		`UPDATE cargo_events SET new_status = 'delivered' WHERE cargo_id = $1`,
		`DELETE FROM cargo_events WHERE cargo_id = $1`,
	} {
		assert.Equal(t, restrictViolation, sqlState(t, sql, cargoID), sql)
	}
	for _, table := range []string{"tracking_entries", "cargo_events"} {
		assert.Equal(t, restrictViolation, sqlState(t, `TRUNCATE `+table+` CASCADE`), table)
	}

	assert.Equal(t, 1, testkit.CountRows(t, db, `SELECT COUNT(*) FROM tracking_entries WHERE cargo_id = $1 AND location = 'x'`, cargoID))
}

func TestSchema_CargoEventsAreUniquePerEventID(t *testing.T) {
	cargoID := insertCargo(t, insertVessel(t))
	eventID := uuid.New()
	insert := `INSERT INTO cargo_events (id, cargo_id, old_status, new_status, occurred_at) VALUES ($1, $2, 'pending', 'in_transit', now())`

	require.Empty(t, sqlState(t, insert, eventID, cargoID))
	assert.Equal(t, uniqueViolation, sqlState(t, insert, eventID, cargoID))
	assert.Equal(t, foreignKeyViolation, sqlState(t, insert, uuid.New(), uuid.New()), "events must reference a real cargo")
}

func TestSchema_IdempotencyKeysAreUniquePerScope(t *testing.T) {
	insert := `INSERT INTO idempotency_keys (scope, key, request_hash, status_code, response_body) VALUES ($1, 'k', 'h', 201, '')`
	key := uuid.NewString()
	insertKey := strings.Replace(insert, "'k'", "'"+key+"'", 1)

	require.Empty(t, sqlState(t, insertKey, "create_cargo"))
	assert.Equal(t, uniqueViolation, sqlState(t, insertKey, "create_cargo"))
	assert.Empty(t, sqlState(t, insertKey, "create_vessel"), "the same key in another scope is independent")
}
