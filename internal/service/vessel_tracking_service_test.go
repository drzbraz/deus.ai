package service_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drzbraz/deus.ai/internal/domain"
	"github.com/drzbraz/deus.ai/internal/service"
)

func TestVesselService_CreateGetList(t *testing.T) {
	store := newMemStore()
	svc := service.NewVesselService(store.repositories(), fixedClock)

	vessel, err := svc.Create(context.Background(), service.CreateVesselInput{Name: "Maersk", CapacityKg: 500, CurrentLocation: "Lisbon"})
	require.NoError(t, err)
	assert.Equal(t, fixedNow, vessel.CreatedAt)

	got, err := svc.Get(context.Background(), vessel.ID)
	require.NoError(t, err)
	assert.Equal(t, "Maersk", got.Name)

	list, err := svc.List(context.Background(), domain.Page{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

func TestVesselService_DuplicateNameIsAConflict(t *testing.T) {
	store := newMemStore()
	svc := service.NewVesselService(store.repositories(), fixedClock)
	input := service.CreateVesselInput{Name: "Maersk", CapacityKg: 500, CurrentLocation: "Lisbon"}

	_, err := svc.Create(context.Background(), input)
	require.NoError(t, err)
	_, err = svc.Create(context.Background(), input)

	assert.ErrorIs(t, err, domain.ErrAlreadyExists)
}

func TestVesselService_Errors(t *testing.T) {
	store := newMemStore()
	svc := service.NewVesselService(store.repositories(), fixedClock)

	_, err := svc.Create(context.Background(), service.CreateVesselInput{})
	assert.ErrorIs(t, err, domain.ErrValidation)

	_, err = svc.Get(context.Background(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrNotFound)

	_, err = svc.UpdateLocation(context.Background(), uuid.New(), "Porto")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	store.failures["vessels.list"] = domain.ErrUnavailable
	_, err = svc.List(context.Background(), domain.Page{})
	assert.ErrorIs(t, err, domain.ErrUnavailable)
}

func TestVesselService_UpdateLocation(t *testing.T) {
	store := newMemStore()
	svc := service.NewVesselService(store.repositories(), fixedClock)
	vessel, err := svc.Create(context.Background(), service.CreateVesselInput{Name: "Maersk", CapacityKg: 500, CurrentLocation: "Lisbon"})
	require.NoError(t, err)

	updated, err := svc.UpdateLocation(context.Background(), vessel.ID, "Porto")
	require.NoError(t, err)
	assert.Equal(t, "Porto", updated.CurrentLocation)

	_, err = svc.UpdateLocation(context.Background(), vessel.ID, "")
	assert.ErrorIs(t, err, domain.ErrValidation)
	assert.Equal(t, "Porto", store.vessels[vessel.ID].CurrentLocation)

	store.failures["vessels.update"] = domain.ErrUnavailable
	_, err = svc.UpdateLocation(context.Background(), vessel.ID, "Faro")
	assert.ErrorIs(t, err, domain.ErrUnavailable)
}

func TestTrackingService_RecordMovementAndHistory(t *testing.T) {
	f := newCargoFixture(t, 100)
	cargo := f.createCargo(t, 10)
	svc := service.NewTrackingService(f.store.repositories(), fixedClock)

	entry, err := svc.RecordMovement(context.Background(), service.RecordMovementInput{CargoID: cargo.ID, Location: "Gibraltar", Note: "passing"})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusPending, entry.Status)

	history, err := svc.History(context.Background(), cargo.ID)
	require.NoError(t, err)
	require.Len(t, history, 2)
	assert.Equal(t, "Gibraltar", history[1].Location)
}

func TestTrackingService_DeliveredCargoAcceptsNoMovements(t *testing.T) {
	f := newCargoFixture(t, 100)
	cargo := f.createCargo(t, 10)
	advance(t, f, cargo.ID, domain.StatusInTransit, domain.StatusDelivered)
	svc := service.NewTrackingService(f.store.repositories(), fixedClock)

	_, err := svc.RecordMovement(context.Background(), service.RecordMovementInput{CargoID: cargo.ID, Location: "Somewhere"})

	assert.ErrorIs(t, err, domain.ErrCargoDelivered)
}

func TestTrackingService_Errors(t *testing.T) {
	f := newCargoFixture(t, 100)
	cargo := f.createCargo(t, 10)
	svc := service.NewTrackingService(f.store.repositories(), fixedClock)

	_, err := svc.RecordMovement(context.Background(), service.RecordMovementInput{CargoID: cargo.ID})
	assert.ErrorIs(t, err, domain.ErrValidation)

	_, err = svc.RecordMovement(context.Background(), service.RecordMovementInput{CargoID: uuid.New(), Location: "x"})
	assert.ErrorIs(t, err, domain.ErrNotFound)

	_, err = svc.History(context.Background(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrNotFound)

	_, err = svc.Events(context.Background(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrNotFound)

	f.store.failures["tracking.list"] = domain.ErrUnavailable
	_, err = svc.History(context.Background(), cargo.ID)
	assert.ErrorIs(t, err, domain.ErrUnavailable)

	f.store.failures["events.list"] = domain.ErrUnavailable
	_, err = svc.Events(context.Background(), cargo.ID)
	assert.ErrorIs(t, err, domain.ErrUnavailable)
}

func TestTrackingService_Events(t *testing.T) {
	f := newCargoFixture(t, 100)
	cargo := f.createCargo(t, 10)
	f.store.events = append(f.store.events, domain.CargoEvent{StatusChanged: domain.StatusChanged{CargoID: cargo.ID}})
	svc := service.NewTrackingService(f.store.repositories(), fixedClock)

	events, err := svc.Events(context.Background(), cargo.ID)

	require.NoError(t, err)
	assert.Len(t, events, 1)
}
