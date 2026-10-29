//go:build e2e

package e2e

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	. "github.com/drzbraz/deus.ai/tests/testkit"
)

func TestConcurrency_SameTransitionRacesExactlyOneWins(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()

	responses := Parallel(20, func(int) Response { return c.ChangeStatus(cargo.ID, InTransit) })

	assert.Equal(t, map[int]int{http.StatusOK: 1, http.StatusConflict: 19}, StatusCounts(responses))
	assert.Len(t, c.History(cargo.ID), 2)
	assert.Equal(t, 1, CountRows(t, stack.DB, `SELECT COUNT(*) FROM outbox WHERE aggregate_id = $1`, cargo.ID))
	c.WaitForEvents(cargo.ID, 1)
	c.AssertEventCountStays(cargo.ID, 1)
}

func TestConcurrency_MixedTransitionsStayConsistent(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()

	responses := Parallel(20, func(i int) Response {
		if i%2 == 0 {
			return c.ChangeStatus(cargo.ID, InTransit)
		}
		return c.ChangeStatus(cargo.ID, Delivered)
	})

	successes := StatusCounts(responses)[http.StatusOK]
	require.GreaterOrEqual(t, successes, 1, "in_transit must win once")
	require.LessOrEqual(t, successes, 2)

	final := c.GetCargo(cargo.ID)
	assert.Equal(t, 1+successes, final.Version)
	history := c.History(cargo.ID)
	assert.Equal(t, []string{Pending, InTransit, Delivered}[:1+successes], statuses(history), "history is a valid prefix of the lifecycle")
	c.WaitForEvents(cargo.ID, successes)
}

func TestConcurrency_CapacityIsNeverOverbooked(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)

	responses := Parallel(25, func(int) Response {
		return c.Post("/api/v1/cargoes", map[string]any{"name": "box", "weight_kg": 10, "vessel_id": vessel.ID})
	})

	assert.Equal(t, map[int]int{http.StatusCreated: 10, http.StatusConflict: 15}, StatusCounts(responses))
	got := c.GetVessel(vessel.ID)
	assert.Equal(t, 100.0, got.LoadKg)
	assert.Zero(t, got.AvailableCapacityKg)
}

func TestConcurrency_MixedWeightsNeverExceedCapacity(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)
	weights := []float64{7, 13, 21, 5, 33, 2, 17, 40, 9, 11, 3, 29}

	Parallel(len(weights), func(i int) Response {
		return c.Post("/api/v1/cargoes", map[string]any{"name": "box", "weight_kg": weights[i], "vessel_id": vessel.ID})
	})

	assert.LessOrEqual(t, c.GetVessel(vessel.ID).LoadKg, 100.0)
}

func TestConcurrency_MovementsRacingDeliveryNeverFollowIt(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoInStatus(InTransit)

	responses := Parallel(21, func(i int) Response {
		if i == 10 {
			return c.ChangeStatus(cargo.ID, Delivered)
		}
		return c.Post(CargoPath(cargo.ID)+"/tracking", map[string]any{"location": "sea"})
	})

	counts := StatusCounts(responses)
	assert.Equal(t, 1, counts[http.StatusOK], "delivery succeeds")
	assert.Equal(t, 20, counts[http.StatusCreated]+counts[http.StatusConflict], "movements either land before delivery or are rejected")

	history := c.History(cargo.ID)
	assert.Equal(t, Delivered, history[len(history)-1].Status, "nothing is recorded after delivery")
	assert.Len(t, history, 2+counts[http.StatusCreated]+1)
}

func TestConcurrency_OptimisticLockingRace(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()

	responses := Parallel(15, func(int) Response { return c.ChangeStatus(cargo.ID, InTransit, IfMatch(`"1"`)) })

	assert.Equal(t, map[int]int{http.StatusOK: 1, http.StatusPreconditionFailed: 14}, StatusCounts(responses))
}

func TestConcurrency_DuplicateVesselNames(t *testing.T) {
	c := api(t)
	name := UniqueName("race")

	responses := Parallel(15, func(int) Response {
		return c.Post("/api/v1/vessels", map[string]any{"name": name, "capacity_kg": 1, "current_location": "x"})
	})

	assert.Equal(t, map[int]int{http.StatusCreated: 1, http.StatusConflict: 14}, StatusCounts(responses))
}

func TestConcurrency_SimultaneousDuplicatePostsWithSameKey(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)
	key := IdempotencyKey(uuid.NewString())
	body := map[string]any{"name": "Fragile", "weight_kg": 5, "vessel_id": vessel.ID}

	responses := Parallel(25, func(int) Response { return c.Post("/api/v1/cargoes", body, key) })

	for _, r := range responses {
		require.Equal(t, http.StatusCreated, r.Status, r.String())
		assert.Equal(t, responses[0].String(), r.String())
	}
	assert.Equal(t, 1, cargoCount(t, vessel.ID))
	assert.Equal(t, 5.0, c.GetVessel(vessel.ID).LoadKg)
}

func TestConcurrency_SimultaneousStatusChangesWithSameKey(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()
	key := IdempotencyKey(uuid.NewString())

	responses := Parallel(20, func(int) Response { return c.ChangeStatus(cargo.ID, InTransit, key) })

	assert.Equal(t, map[int]int{http.StatusOK: 20}, StatusCounts(responses), "every duplicate sees the winner's answer")
	assert.Len(t, c.History(cargo.ID), 2)
	c.WaitForEvents(cargo.ID, 1)
	c.AssertEventCountStays(cargo.ID, 1)
}

func TestConcurrency_SameKeyDifferentBodiesAtOnce(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)
	key := IdempotencyKey(uuid.NewString())

	responses := Parallel(20, func(i int) Response {
		name := []string{"A", "B"}[i%2]
		return c.Post("/api/v1/cargoes", map[string]any{"name": name, "weight_kg": 1, "vessel_id": vessel.ID}, key)
	})

	counts := StatusCounts(responses)
	assert.Equal(t, 10, counts[http.StatusCreated], "the winner's twins replay it")
	assert.Equal(t, 10, counts[http.StatusUnprocessableEntity], "the others are rejected as key reuse")
	assert.Equal(t, 1, cargoCount(t, vessel.ID))
}

func TestConcurrency_ReadersNeverSeeTornState(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()
	expectedVersion := map[string]int{Pending: 1, InTransit: 2, Delivered: 3}

	responses := Parallel(60, func(i int) Response {
		switch i {
		case 20:
			return c.ChangeStatus(cargo.ID, InTransit)
		case 40:
			return c.ChangeStatus(cargo.ID, Delivered)
		default:
			return c.Get(CargoPath(cargo.ID))
		}
	})

	for i, r := range responses {
		if i == 20 || i == 40 || r.Status != http.StatusOK {
			continue
		}
		seen := Data[Cargo](t, r)
		assert.Equal(t, expectedVersion[seen.Status], seen.Version, "status and version always change together")
	}
}

func TestConcurrency_IndependentCargoesDoNotBlockEachOther(t *testing.T) {
	c := api(t)
	cargoes := make([]Cargo, 15)
	for i := range cargoes {
		cargoes[i] = c.NewCargoOnFreshVessel()
	}

	responses := Parallel(len(cargoes), func(i int) Response { return c.ChangeStatus(cargoes[i].ID, InTransit) })

	assert.Equal(t, map[int]int{http.StatusOK: len(cargoes)}, StatusCounts(responses))
}
