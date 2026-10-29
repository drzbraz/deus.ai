package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/drzbraz/deus.ai/internal/domain"
	"github.com/drzbraz/deus.ai/internal/idempotency"
	"github.com/drzbraz/deus.ai/internal/service"
	"github.com/drzbraz/deus.ai/internal/transport/httpapi"
)

// Fakes use function fields so each test states only the behaviour it needs.

type fakeVessels struct {
	create         func(service.CreateVesselInput) (domain.Vessel, error)
	get            func(uuid.UUID) (domain.Vessel, error)
	list           func(domain.Page) ([]domain.Vessel, error)
	updateLocation func(uuid.UUID, string) (domain.Vessel, error)
}

func (f fakeVessels) Create(_ context.Context, in service.CreateVesselInput) (domain.Vessel, error) {
	return f.create(in)
}
func (f fakeVessels) Get(_ context.Context, id uuid.UUID) (domain.Vessel, error) { return f.get(id) }
func (f fakeVessels) List(_ context.Context, p domain.Page) ([]domain.Vessel, error) {
	return f.list(p)
}
func (f fakeVessels) UpdateLocation(_ context.Context, id uuid.UUID, loc string) (domain.Vessel, error) {
	return f.updateLocation(id, loc)
}

type fakeCargoes struct {
	create       func(service.CreateCargoInput) (domain.Cargo, error)
	get          func(context.Context, uuid.UUID) (domain.Cargo, error)
	list         func(domain.CargoFilter) ([]domain.Cargo, error)
	updateStatus func(service.UpdateStatusInput) (domain.Cargo, error)
}

func (f fakeCargoes) Create(_ context.Context, in service.CreateCargoInput) (domain.Cargo, error) {
	return f.create(in)
}
func (f fakeCargoes) Get(ctx context.Context, id uuid.UUID) (domain.Cargo, error) {
	return f.get(ctx, id)
}
func (f fakeCargoes) List(_ context.Context, filter domain.CargoFilter) ([]domain.Cargo, error) {
	return f.list(filter)
}
func (f fakeCargoes) UpdateStatus(_ context.Context, in service.UpdateStatusInput) (domain.Cargo, error) {
	return f.updateStatus(in)
}

type fakeTracking struct {
	record  func(service.RecordMovementInput) (domain.TrackingEntry, error)
	history func(uuid.UUID) ([]domain.TrackingEntry, error)
	events  func(uuid.UUID) ([]domain.CargoEvent, error)
}

func (f fakeTracking) RecordMovement(_ context.Context, in service.RecordMovementInput) (domain.TrackingEntry, error) {
	return f.record(in)
}
func (f fakeTracking) History(_ context.Context, id uuid.UUID) ([]domain.TrackingEntry, error) {
	return f.history(id)
}
func (f fakeTracking) Events(_ context.Context, id uuid.UUID) ([]domain.CargoEvent, error) {
	return f.events(id)
}

// memIdempotency mimics the transactional store: claims made in a failed
// transaction disappear, like a rollback.
type memIdempotency struct {
	records  map[string]idempotency.Record
	pending  map[string]bool
	claimErr error
}

func newMemIdempotency() *memIdempotency {
	return &memIdempotency{records: map[string]idempotency.Record{}, pending: map[string]bool{}}
}

func (m *memIdempotency) Claim(_ context.Context, scope, key, hash string) (idempotency.Record, bool, error) {
	if m.claimErr != nil {
		return idempotency.Record{}, false, m.claimErr
	}
	if rec, ok := m.records[scope+key]; ok {
		return rec, false, nil
	}
	m.pending[scope+key] = true
	return idempotency.Record{}, true, nil
}

func (m *memIdempotency) Complete(_ context.Context, rec idempotency.Record) error {
	m.records[rec.Scope+rec.Key] = rec
	delete(m.pending, rec.Scope+rec.Key)
	return nil
}

func (m *memIdempotency) DeleteExpired(context.Context, time.Time) (int64, error) { return 0, nil }

type memTx struct{ store *memIdempotency }

func (t memTx) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	err := fn(ctx)
	if err != nil {
		for k := range t.store.pending {
			delete(t.store.pending, k)
		}
	}
	return err
}

type testServer struct {
	vessels     fakeVessels
	cargoes     fakeCargoes
	tracking    fakeTracking
	idempotency *memIdempotency
	checks      []httpapi.HealthCheck
	timeout     time.Duration
}

func newTestServer() *testServer {
	return &testServer{idempotency: newMemIdempotency(), timeout: time.Second}
}

func (s *testServer) handler() http.Handler {
	return httpapi.NewRouter(httpapi.Dependencies{
		Vessels:          s.vessels,
		Cargoes:          s.cargoes,
		Tracking:         s.tracking,
		Tx:               memTx{s.idempotency},
		IdempotencyStore: s.idempotency,
		HealthChecks:     s.checks,
		Logger:           zap.NewNop(),
		RequestTimeout:   s.timeout,
	})
}

func (s *testServer) do(t *testing.T, method, path, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	require.Zero(t, len(headers)%2, "headers must be name/value pairs")
	for i := 0; i < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	return rec
}

var (
	now      = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	vesselID = uuid.MustParse("0192d6c0-0000-7000-8000-000000000001")
	cargoID  = uuid.MustParse("0192d6c0-0000-7000-8000-000000000002")
)

func sampleVessel() domain.Vessel {
	return domain.Vessel{ID: vesselID, Name: "Ever Given", CapacityKg: 1000, LoadKg: 250, CurrentLocation: "Rotterdam", CreatedAt: now, UpdatedAt: now}
}

func sampleCargo() domain.Cargo {
	return domain.Cargo{ID: cargoID, Name: "Coffee", WeightKg: 10, Status: domain.StatusPending, VesselID: vesselID, Version: 1, CreatedAt: now, UpdatedAt: now}
}
