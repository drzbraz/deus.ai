package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drzbraz/deus.ai/internal/domain"
)

var now = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func TestNewVessel_TrimsAndBuilds(t *testing.T) {
	vessel, err := domain.NewVessel("  Ever Given ", 1000, " Rotterdam ", now)

	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, vessel.ID)
	assert.Equal(t, "Ever Given", vessel.Name)
	assert.Equal(t, "Rotterdam", vessel.CurrentLocation)
	assert.Equal(t, now, vessel.CreatedAt)
}

func TestNewVessel_ReportsEveryInvalidField(t *testing.T) {
	_, err := domain.NewVessel("   ", 0, "", now)

	var verr *domain.ValidationError
	require.True(t, errors.As(err, &verr))
	assert.Equal(t, map[string]string{
		"name":             "is required",
		"capacity_kg":      "must be greater than 0",
		"current_location": "is required",
	}, verr.Fields)
	assert.Contains(t, err.Error(), "capacity_kg: must be greater than 0")
}

func TestNewVessel_RejectsOversizedValues(t *testing.T) {
	_, err := domain.NewVessel(strings.Repeat("x", 201), domain.MaxWeightKg+1, "Lisbon", now)

	var verr *domain.ValidationError
	require.True(t, errors.As(err, &verr))
	assert.Contains(t, verr.Fields, "name")
	assert.Contains(t, verr.Fields, "capacity_kg")
}

func TestVessel_MoveTo(t *testing.T) {
	vessel, err := domain.NewVessel("Maersk", 10, "Lisbon", now)
	require.NoError(t, err)

	later := now.Add(time.Hour)
	require.NoError(t, vessel.MoveTo(" Porto ", later))
	assert.Equal(t, "Porto", vessel.CurrentLocation)
	assert.Equal(t, later, vessel.UpdatedAt)

	assert.ErrorIs(t, vessel.MoveTo(" ", later), domain.ErrValidation)
	assert.Equal(t, "Porto", vessel.CurrentLocation, "location must not change on invalid input")
}

func TestVessel_CanLoad(t *testing.T) {
	vessel := domain.Vessel{CapacityKg: 100}

	assert.True(t, vessel.CanLoad(60, 40), "exactly full is allowed")
	assert.False(t, vessel.CanLoad(60, 40.01))
}

func TestNewCargo_StartsPending(t *testing.T) {
	cargo, err := domain.NewCargo(domain.NewCargoParams{
		Name: " Coffee ", Description: " beans ", WeightKg: 50, VesselID: uuid.New(),
	}, now)

	require.NoError(t, err)
	assert.Equal(t, domain.StatusPending, cargo.Status)
	assert.Equal(t, 1, cargo.Version)
	assert.Equal(t, "Coffee", cargo.Name)
	assert.Equal(t, "beans", cargo.Description)
}

func TestNewCargo_Validation(t *testing.T) {
	_, err := domain.NewCargo(domain.NewCargoParams{
		Description: strings.Repeat("d", domain.MaxNoteLength+1), WeightKg: -1,
	}, now)

	var verr *domain.ValidationError
	require.True(t, errors.As(err, &verr))
	assert.ElementsMatch(t, []string{"name", "description", "weight_kg", "vessel_id"}, keys(verr.Fields))
}

func TestCargo_ChangeStatus_EmitsEventAndBumpsVersion(t *testing.T) {
	cargo := pendingCargo(t)
	later := now.Add(time.Minute)

	event, err := cargo.ChangeStatus(domain.StatusInTransit, later)

	require.NoError(t, err)
	assert.Equal(t, domain.StatusInTransit, cargo.Status)
	assert.Equal(t, 2, cargo.Version)
	assert.Equal(t, later, cargo.UpdatedAt)
	assert.Equal(t, cargo.ID, event.CargoID)
	assert.Equal(t, domain.StatusPending, event.OldStatus)
	assert.Equal(t, domain.StatusInTransit, event.NewStatus)
	assert.Equal(t, later, event.OccurredAt)
	assert.NotEqual(t, uuid.Nil, event.EventID)
}

func TestCargo_ChangeStatus_RejectsBackwardTransitionWithoutMutating(t *testing.T) {
	cargo := pendingCargo(t)
	_, err := cargo.ChangeStatus(domain.StatusInTransit, now)
	require.NoError(t, err)

	_, err = cargo.ChangeStatus(domain.StatusPending, now)

	assert.ErrorIs(t, err, domain.ErrInvalidTransition)
	assert.Equal(t, domain.StatusInTransit, cargo.Status)
	assert.Equal(t, 2, cargo.Version)
}

func TestNewTrackingEntry(t *testing.T) {
	cargo := pendingCargo(t)

	entry, err := domain.NewTrackingEntry(cargo, " Suez ", " canal ", now)
	require.NoError(t, err)
	assert.Equal(t, cargo.ID, entry.CargoID)
	assert.Equal(t, domain.StatusPending, entry.Status)
	assert.Equal(t, "Suez", entry.Location)
	assert.Equal(t, "canal", entry.Note)

	_, err = domain.NewTrackingEntry(cargo, "", strings.Repeat("n", domain.MaxNoteLength+1), now)
	var verr *domain.ValidationError
	require.True(t, errors.As(err, &verr))
	assert.ElementsMatch(t, []string{"location", "note"}, keys(verr.Fields))
}

func pendingCargo(t *testing.T) domain.Cargo {
	t.Helper()
	cargo, err := domain.NewCargo(domain.NewCargoParams{Name: "Tea", WeightKg: 10, VesselID: uuid.New()}, now)
	require.NoError(t, err)
	return cargo
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
