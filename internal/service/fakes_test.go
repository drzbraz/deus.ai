package service_test

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/drzbraz/deus.ai/internal/domain"
	"github.com/drzbraz/deus.ai/internal/service"
)

// memStore is an in-memory implementation of every repository port. Its
// TxManager snapshots state and restores it when fn fails, mimicking a
// database rollback so tests can assert "nothing was written".
type memStore struct {
	vessels  map[uuid.UUID]domain.Vessel
	cargoes  map[uuid.UUID]domain.Cargo
	tracking []domain.TrackingEntry
	outbox   []domain.StatusChanged
	events   []domain.CargoEvent
	// failures injects an error for a named operation, e.g. "outbox.enqueue".
	failures map[string]error
}

func newMemStore() *memStore {
	return &memStore{
		vessels:  map[uuid.UUID]domain.Vessel{},
		cargoes:  map[uuid.UUID]domain.Cargo{},
		failures: map[string]error{},
	}
}

func (m *memStore) repositories() service.Repositories {
	return service.Repositories{
		Tx:       memTx{m},
		Vessels:  memVessels{m},
		Cargoes:  memCargoes{m},
		Tracking: memTracking{m},
		Outbox:   memOutbox{m},
		Events:   memEvents{m},
	}
}

func (m *memStore) fail(op string) error { return m.failures[op] }

func (m *memStore) loadOf(vesselID uuid.UUID) float64 {
	var load float64
	for _, c := range m.cargoes {
		if c.VesselID == vesselID && c.Status != domain.StatusDelivered {
			load += c.WeightKg
		}
	}
	return load
}

type memTx struct{ m *memStore }

func (t memTx) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if err := t.m.fail("tx.begin"); err != nil {
		return err
	}
	vessels, cargoes := maps.Clone(t.m.vessels), maps.Clone(t.m.cargoes)
	tracking, outbox := slices.Clone(t.m.tracking), slices.Clone(t.m.outbox)

	if err := fn(ctx); err != nil {
		t.m.vessels, t.m.cargoes, t.m.tracking, t.m.outbox = vessels, cargoes, tracking, outbox
		return err
	}
	return nil
}

type memVessels struct{ m *memStore }

func (r memVessels) Create(_ context.Context, v domain.Vessel) error {
	if err := r.m.fail("vessels.create"); err != nil {
		return err
	}
	for _, existing := range r.m.vessels {
		if existing.Name == v.Name {
			return domain.ErrAlreadyExists
		}
	}
	r.m.vessels[v.ID] = v
	return nil
}

func (r memVessels) Get(_ context.Context, id uuid.UUID) (domain.Vessel, error) {
	if err := r.m.fail("vessels.get"); err != nil {
		return domain.Vessel{}, err
	}
	v, ok := r.m.vessels[id]
	if !ok {
		return domain.Vessel{}, domain.ErrNotFound
	}
	v.LoadKg = r.m.loadOf(id)
	return v, nil
}

func (r memVessels) GetForUpdate(ctx context.Context, id uuid.UUID) (domain.Vessel, error) {
	return r.Get(ctx, id)
}

func (r memVessels) List(_ context.Context, _ domain.Page) ([]domain.Vessel, error) {
	if err := r.m.fail("vessels.list"); err != nil {
		return nil, err
	}
	return slices.Collect(maps.Values(r.m.vessels)), nil
}

func (r memVessels) Update(_ context.Context, v domain.Vessel) error {
	if err := r.m.fail("vessels.update"); err != nil {
		return err
	}
	r.m.vessels[v.ID] = v
	return nil
}

type memCargoes struct{ m *memStore }

func (r memCargoes) Create(_ context.Context, c domain.Cargo) error {
	if err := r.m.fail("cargoes.create"); err != nil {
		return err
	}
	r.m.cargoes[c.ID] = c
	return nil
}

func (r memCargoes) Get(_ context.Context, id uuid.UUID) (domain.Cargo, error) {
	if err := r.m.fail("cargoes.get"); err != nil {
		return domain.Cargo{}, err
	}
	c, ok := r.m.cargoes[id]
	if !ok {
		return domain.Cargo{}, domain.ErrNotFound
	}
	return c, nil
}

func (r memCargoes) GetForUpdate(ctx context.Context, id uuid.UUID) (domain.Cargo, error) {
	return r.Get(ctx, id)
}

func (r memCargoes) List(_ context.Context, _ domain.CargoFilter) ([]domain.Cargo, error) {
	if err := r.m.fail("cargoes.list"); err != nil {
		return nil, err
	}
	return slices.Collect(maps.Values(r.m.cargoes)), nil
}

func (r memCargoes) Update(_ context.Context, c domain.Cargo) error {
	if err := r.m.fail("cargoes.update"); err != nil {
		return err
	}
	r.m.cargoes[c.ID] = c
	return nil
}

type memTracking struct{ m *memStore }

func (r memTracking) Append(_ context.Context, e domain.TrackingEntry) error {
	if err := r.m.fail("tracking.append"); err != nil {
		return err
	}
	r.m.tracking = append(r.m.tracking, e)
	return nil
}

func (r memTracking) ListByCargo(_ context.Context, cargoID uuid.UUID) ([]domain.TrackingEntry, error) {
	if err := r.m.fail("tracking.list"); err != nil {
		return nil, err
	}
	var out []domain.TrackingEntry
	for _, e := range r.m.tracking {
		if e.CargoID == cargoID {
			out = append(out, e)
		}
	}
	return out, nil
}

type memOutbox struct{ m *memStore }

func (r memOutbox) Enqueue(_ context.Context, e domain.StatusChanged) error {
	if err := r.m.fail("outbox.enqueue"); err != nil {
		return err
	}
	r.m.outbox = append(r.m.outbox, e)
	return nil
}

type memEvents struct{ m *memStore }

func (r memEvents) ListByCargo(_ context.Context, cargoID uuid.UUID) ([]domain.CargoEvent, error) {
	if err := r.m.fail("events.list"); err != nil {
		return nil, err
	}
	var out []domain.CargoEvent
	for _, e := range r.m.events {
		if e.CargoID == cargoID {
			out = append(out, e)
		}
	}
	return out, nil
}

type transitionSpy struct {
	recorded [][2]domain.Status
}

func (s *transitionSpy) RecordTransition(_ context.Context, from, to domain.Status) {
	s.recorded = append(s.recorded, [2]domain.Status{from, to})
}

var fixedNow = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func fixedClock() time.Time { return fixedNow }
