package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drzbraz/deus.ai/internal/domain"
	"github.com/drzbraz/deus.ai/internal/service"
)

type cargoFixture struct {
	store   *memStore
	spy     *transitionSpy
	svc     *service.CargoService
	vessels *service.VesselService
	vessel  domain.Vessel
}

func newCargoFixture(t *testing.T, capacityKg float64) cargoFixture {
	t.Helper()
	store := newMemStore()
	spy := &transitionSpy{}
	vessels := service.NewVesselService(store.repositories(), fixedClock)

	vessel, err := vessels.Create(context.Background(), service.CreateVesselInput{
		Name: "Ever Given", CapacityKg: capacityKg, CurrentLocation: "Rotterdam",
	})
	require.NoError(t, err)

	return cargoFixture{
		store:   store,
		spy:     spy,
		svc:     service.NewCargoService(store.repositories(), fixedClock, spy),
		vessels: vessels,
		vessel:  vessel,
	}
}

func (f cargoFixture) createCargo(t *testing.T, weight float64) domain.Cargo {
	t.Helper()
	cargo, err := f.svc.Create(context.Background(), service.CreateCargoInput{
		Name: "Coffee", WeightKg: weight, VesselID: f.vessel.ID,
	})
	require.NoError(t, err)
	return cargo
}

func TestCreateCargo_StartsPendingWithInitialTrackingEntry(t *testing.T) {
	f := newCargoFixture(t, 1000)

	cargo := f.createCargo(t, 100)

	assert.Equal(t, domain.StatusPending, cargo.Status)
	require.Len(t, f.store.tracking, 1)
	assert.Equal(t, "Rotterdam", f.store.tracking[0].Location)
	assert.Equal(t, domain.StatusPending, f.store.tracking[0].Status)
	assert.Empty(t, f.store.outbox, "creation is not a status change")
}

func TestCreateCargo_RejectsWhenVesselCapacityExceeded(t *testing.T) {
	f := newCargoFixture(t, 100)
	f.createCargo(t, 60)

	_, err := f.svc.Create(context.Background(), service.CreateCargoInput{
		Name: "Tea", WeightKg: 41, VesselID: f.vessel.ID,
	})

	assert.ErrorIs(t, err, domain.ErrCapacityExceeded)
	assert.Len(t, f.store.cargoes, 1)
}

func TestCreateCargo_DeliveredCargoFreesCapacity(t *testing.T) {
	f := newCargoFixture(t, 100)
	cargo := f.createCargo(t, 100)
	advance(t, f, cargo.ID, domain.StatusInTransit, domain.StatusDelivered)

	f.createCargo(t, 100)
}

func TestCreateCargo_UnknownVesselIsAValidationError(t *testing.T) {
	f := newCargoFixture(t, 100)

	_, err := f.svc.Create(context.Background(), service.CreateCargoInput{
		Name: "Tea", WeightKg: 1, VesselID: uuid.New(),
	})

	var verr *domain.ValidationError
	require.ErrorAs(t, err, &verr)
	assert.Contains(t, verr.Fields, "vessel_id")
}

func TestCreateCargo_InvalidInputNeverTouchesStorage(t *testing.T) {
	f := newCargoFixture(t, 100)
	f.store.failures["tx.begin"] = errors.New("must not be called")

	_, err := f.svc.Create(context.Background(), service.CreateCargoInput{Name: "", WeightKg: 1, VesselID: f.vessel.ID})

	assert.ErrorIs(t, err, domain.ErrValidation)
}

func TestCreateCargo_RollsBackWhenTrackingFails(t *testing.T) {
	f := newCargoFixture(t, 100)
	f.store.failures["tracking.append"] = domain.ErrUnavailable

	_, err := f.svc.Create(context.Background(), service.CreateCargoInput{Name: "Tea", WeightKg: 1, VesselID: f.vessel.ID})

	assert.ErrorIs(t, err, domain.ErrUnavailable)
	assert.Empty(t, f.store.cargoes, "cargo insert must be rolled back")
}

func TestUpdateStatus_ForwardTransitionWritesTrackingAndOutbox(t *testing.T) {
	f := newCargoFixture(t, 100)
	cargo := f.createCargo(t, 10)

	updated, err := f.svc.UpdateStatus(context.Background(), service.UpdateStatusInput{
		CargoID: cargo.ID, Status: "in_transit", Location: "Suez", Note: "departed",
	})

	require.NoError(t, err)
	assert.Equal(t, domain.StatusInTransit, updated.Status)
	assert.Equal(t, 2, updated.Version)

	require.Len(t, f.store.tracking, 2)
	assert.Equal(t, "Suez", f.store.tracking[1].Location)
	assert.Equal(t, domain.StatusInTransit, f.store.tracking[1].Status)

	require.Len(t, f.store.outbox, 1)
	assert.Equal(t, domain.StatusPending, f.store.outbox[0].OldStatus)
	assert.Equal(t, domain.StatusInTransit, f.store.outbox[0].NewStatus)
	assert.Equal(t, [][2]domain.Status{{domain.StatusPending, domain.StatusInTransit}}, f.spy.recorded)
}

func TestUpdateStatus_DefaultsLocationToVesselPosition(t *testing.T) {
	f := newCargoFixture(t, 100)
	cargo := f.createCargo(t, 10)
	_, err := f.vessels.UpdateLocation(context.Background(), f.vessel.ID, "Singapore")
	require.NoError(t, err)

	advance(t, f, cargo.ID, domain.StatusInTransit)

	assert.Equal(t, "Singapore", f.store.tracking[1].Location)
}

func TestUpdateStatus_RejectedTransitionsEmitNothing(t *testing.T) {
	tests := []struct {
		name    string
		history []domain.Status
		target  string
	}{
		{"same status", nil, "pending"},
		{"skip a step", nil, "delivered"},
		{"backward", []domain.Status{domain.StatusInTransit}, "pending"},
		{"after delivery", []domain.Status{domain.StatusInTransit, domain.StatusDelivered}, "in_transit"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCargoFixture(t, 100)
			cargo := f.createCargo(t, 10)
			advance(t, f, cargo.ID, tt.history...)
			outboxBefore, trackingBefore := len(f.store.outbox), len(f.store.tracking)

			_, err := f.svc.UpdateStatus(context.Background(), service.UpdateStatusInput{CargoID: cargo.ID, Status: tt.target})

			assert.ErrorIs(t, err, domain.ErrInvalidTransition)
			assert.Len(t, f.store.outbox, outboxBefore)
			assert.Len(t, f.store.tracking, trackingBefore)
		})
	}
}

func TestUpdateStatus_InvalidStatusValue(t *testing.T) {
	f := newCargoFixture(t, 100)
	cargo := f.createCargo(t, 10)

	_, err := f.svc.UpdateStatus(context.Background(), service.UpdateStatusInput{CargoID: cargo.ID, Status: "lost"})

	assert.ErrorIs(t, err, domain.ErrValidation)
}

func TestUpdateStatus_UnknownCargo(t *testing.T) {
	f := newCargoFixture(t, 100)

	_, err := f.svc.UpdateStatus(context.Background(), service.UpdateStatusInput{CargoID: uuid.New(), Status: "in_transit"})

	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestUpdateStatus_StaleVersionIsRejected(t *testing.T) {
	f := newCargoFixture(t, 100)
	cargo := f.createCargo(t, 10)

	_, err := f.svc.UpdateStatus(context.Background(), service.UpdateStatusInput{
		CargoID: cargo.ID, Status: "in_transit", ExpectedVersion: cargo.Version + 1,
	})
	assert.ErrorIs(t, err, domain.ErrVersionMismatch)

	_, err = f.svc.UpdateStatus(context.Background(), service.UpdateStatusInput{
		CargoID: cargo.ID, Status: "in_transit", ExpectedVersion: cargo.Version,
	})
	assert.NoError(t, err)
}

func TestUpdateStatus_OutboxFailureRollsBackEverything(t *testing.T) {
	f := newCargoFixture(t, 100)
	cargo := f.createCargo(t, 10)
	f.store.failures["outbox.enqueue"] = domain.ErrUnavailable

	_, err := f.svc.UpdateStatus(context.Background(), service.UpdateStatusInput{CargoID: cargo.ID, Status: "in_transit"})

	assert.ErrorIs(t, err, domain.ErrUnavailable)
	assert.Equal(t, domain.StatusPending, f.store.cargoes[cargo.ID].Status, "status change must be rolled back")
	assert.Len(t, f.store.tracking, 1)
	assert.Empty(t, f.spy.recorded, "metrics only count committed transitions")
}

func TestUpdateStatus_InvalidLocationRollsBack(t *testing.T) {
	f := newCargoFixture(t, 100)
	cargo := f.createCargo(t, 10)

	_, err := f.svc.UpdateStatus(context.Background(), service.UpdateStatusInput{
		CargoID: cargo.ID, Status: "in_transit", Location: "   ",
	})

	assert.ErrorIs(t, err, domain.ErrValidation)
	assert.Equal(t, domain.StatusPending, f.store.cargoes[cargo.ID].Status)
}

func TestUpdateStatus_StorageErrorsArePropagated(t *testing.T) {
	for _, op := range []string{"vessels.get", "cargoes.update", "tracking.append"} {
		t.Run(op, func(t *testing.T) {
			f := newCargoFixture(t, 100)
			cargo := f.createCargo(t, 10)
			f.store.failures[op] = domain.ErrUnavailable

			_, err := f.svc.UpdateStatus(context.Background(), service.UpdateStatusInput{CargoID: cargo.ID, Status: "in_transit"})

			assert.ErrorIs(t, err, domain.ErrUnavailable)
			assert.Empty(t, f.store.outbox)
		})
	}
}

func TestGetAndListCargo(t *testing.T) {
	f := newCargoFixture(t, 100)
	cargo := f.createCargo(t, 10)

	got, err := f.svc.Get(context.Background(), cargo.ID)
	require.NoError(t, err)
	assert.Equal(t, cargo, got)

	_, err = f.svc.Get(context.Background(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrNotFound)

	list, err := f.svc.List(context.Background(), domain.CargoFilter{})
	require.NoError(t, err)
	assert.Len(t, list, 1)

	f.store.failures["cargoes.list"] = domain.ErrUnavailable
	_, err = f.svc.List(context.Background(), domain.CargoFilter{})
	assert.ErrorIs(t, err, domain.ErrUnavailable)
}

func advance(t *testing.T, f cargoFixture, cargoID uuid.UUID, statuses ...domain.Status) {
	t.Helper()
	for _, status := range statuses {
		_, err := f.svc.UpdateStatus(context.Background(), service.UpdateStatusInput{CargoID: cargoID, Status: string(status)})
		require.NoError(t, err)
	}
}
