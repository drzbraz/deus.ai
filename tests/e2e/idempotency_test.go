//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	. "github.com/drzbraz/deus.ai/tests/testkit"
)

func cargoCount(t *testing.T, vesselID string) int {
	t.Helper()
	return CountRows(t, stack.DB, `SELECT COUNT(*) FROM cargoes WHERE vessel_id = $1`, vesselID)
}

func TestIdempotency_RetryReplaysTheOriginalResponse(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)
	key := IdempotencyKey(uuid.NewString())
	body := map[string]any{"name": "Coffee", "weight_kg": 10, "vessel_id": vessel.ID}

	first := c.Post("/api/v1/cargoes", body, key)
	second := c.Post("/api/v1/cargoes", body, key)
	third := c.Post("/api/v1/cargoes", body, key)

	require.Equal(t, http.StatusCreated, first.Status)
	for _, replay := range []Response{second, third} {
		assert.Equal(t, http.StatusCreated, replay.Status)
		assert.Equal(t, first.String(), replay.String(), "byte-identical body")
		assert.Equal(t, first.Header.Get("Location"), replay.Header.Get("Location"))
		assert.Equal(t, first.Header.Get("ETag"), replay.Header.Get("ETag"))
		assert.Equal(t, "true", replay.Header.Get("Idempotent-Replayed"))
	}
	assert.Empty(t, first.Header.Get("Idempotent-Replayed"))
	assert.Equal(t, 1, cargoCount(t, vessel.ID))
	assert.Equal(t, 10.0, c.GetVessel(vessel.ID).LoadKg, "capacity consumed once")
}

func TestIdempotency_ReplayReturnsOriginalEvenAfterResourceChanged(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)
	key := IdempotencyKey(uuid.NewString())
	body := map[string]any{"name": "Coffee", "weight_kg": 10, "vessel_id": vessel.ID}
	first := c.Post("/api/v1/cargoes", body, key)
	c.MustChangeStatus(Data[Cargo](t, first).ID, InTransit)

	replay := c.Post("/api/v1/cargoes", body, key)

	assert.Equal(t, first.String(), replay.String(), "a replay is the original answer, not the current state")
}

func TestIdempotency_DifferentKeysCreateDifferentResources(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)
	body := map[string]any{"name": "Coffee", "weight_kg": 10, "vessel_id": vessel.ID}

	a := c.Post("/api/v1/cargoes", body, IdempotencyKey(uuid.NewString()))
	b := c.Post("/api/v1/cargoes", body, IdempotencyKey(uuid.NewString()))

	assert.NotEqual(t, Data[Cargo](t, a).ID, Data[Cargo](t, b).ID)
	assert.Equal(t, 2, cargoCount(t, vessel.ID))
}

func TestIdempotency_WithoutKeyDuplicatesAreNotPrevented(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)
	body := map[string]any{"name": "Coffee", "weight_kg": 10, "vessel_id": vessel.ID}

	c.Post("/api/v1/cargoes", body)
	c.Post("/api/v1/cargoes", body)

	assert.Equal(t, 2, cargoCount(t, vessel.ID), "documented: clients must send Idempotency-Key to be safe on retry")
}

func TestIdempotency_KeyReusedWithDifferentBodyIsRejected(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)
	key := IdempotencyKey(uuid.NewString())
	c.Post("/api/v1/cargoes", map[string]any{"name": "A", "weight_kg": 1, "vessel_id": vessel.ID}, key)

	resp := c.Post("/api/v1/cargoes", map[string]any{"name": "B", "weight_kg": 1, "vessel_id": vessel.ID}, key)

	require.Equal(t, http.StatusUnprocessableEntity, resp.Status)
	assert.Equal(t, "idempotency_key_reused", ErrorOf(t, resp).Code)
	assert.Equal(t, 1, cargoCount(t, vessel.ID))
}

func TestIdempotency_KeyIsBoundToThePath(t *testing.T) {
	c := api(t)
	a, b := c.NewCargoOnFreshVessel(), c.NewCargoOnFreshVessel()
	key := IdempotencyKey(uuid.NewString())

	require.Equal(t, http.StatusOK, c.ChangeStatus(a.ID, InTransit, key).Status)
	resp := c.ChangeStatus(b.ID, InTransit, key)

	assert.Equal(t, http.StatusUnprocessableEntity, resp.Status, "same key + body on another cargo is a different request")
	assert.Equal(t, Pending, c.GetCargo(b.ID).Status)
}

func TestIdempotency_KeysAreScopedPerOperation(t *testing.T) {
	c := api(t)
	shared := uuid.NewString()

	vessel := c.Post("/api/v1/vessels", map[string]any{"name": UniqueName("scoped"), "capacity_kg": 100, "current_location": "x"}, IdempotencyKey(shared))
	require.Equal(t, http.StatusCreated, vessel.Status)
	cargo := c.Post("/api/v1/cargoes", map[string]any{"name": "x", "weight_kg": 1, "vessel_id": Data[Vessel](t, vessel).ID}, IdempotencyKey(shared))

	assert.Equal(t, http.StatusCreated, cargo.Status)
	assert.Empty(t, cargo.Header.Get("Idempotent-Replayed"))
}

func TestIdempotency_ClientErrorsAreReplayed(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)
	key := IdempotencyKey(uuid.NewString())
	invalid := map[string]any{"name": "", "weight_kg": 1, "vessel_id": vessel.ID}

	first := c.Post("/api/v1/cargoes", invalid, key)
	again := c.Post("/api/v1/cargoes", invalid, key)
	fixed := c.Post("/api/v1/cargoes", map[string]any{"name": "ok", "weight_kg": 1, "vessel_id": vessel.ID}, key)

	assert.Equal(t, http.StatusUnprocessableEntity, first.Status)
	assert.Equal(t, first.String(), again.String())
	assert.Equal(t, "true", again.Header.Get("Idempotent-Replayed"))
	assert.Equal(t, "idempotency_key_reused", ErrorOf(t, fixed).Code, "a corrected request needs a new key")
}

func TestIdempotency_ConflictsAreReplayed(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()
	key := IdempotencyKey(uuid.NewString())

	first := c.ChangeStatus(cargo.ID, Delivered, key)
	second := c.ChangeStatus(cargo.ID, Delivered, key)

	assert.Equal(t, http.StatusConflict, first.Status)
	assert.Equal(t, http.StatusConflict, second.Status)
	assert.Equal(t, "true", second.Header.Get("Idempotent-Replayed"))
}

func TestIdempotency_StatusChangeRetryDoesNotEmitTwice(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()
	key := IdempotencyKey(uuid.NewString())

	first := c.ChangeStatus(cargo.ID, InTransit, key)
	retry := c.ChangeStatus(cargo.ID, InTransit, key)

	assert.Equal(t, http.StatusOK, first.Status)
	assert.Equal(t, http.StatusOK, retry.Status, "without the key the retry would be a 409")
	assert.Equal(t, first.String(), retry.String())
	assert.Len(t, c.History(cargo.ID), 2)
	c.WaitForEvents(cargo.ID, 1)
	c.AssertEventCountStays(cargo.ID, 1)
}

func TestIdempotency_MovementRetryRecordsOnce(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()
	key := IdempotencyKey(uuid.NewString())
	body := map[string]any{"location": "Azores"}

	c.Post(CargoPath(cargo.ID)+"/tracking", body, key)
	c.Post(CargoPath(cargo.ID)+"/tracking", body, key)

	assert.Len(t, c.History(cargo.ID), 2)
}

func TestIdempotency_KeyLength(t *testing.T) {
	c := api(t)
	body := func() map[string]any {
		return map[string]any{"name": UniqueName("len"), "capacity_kg": 1, "current_location": "x"}
	}

	assert.Equal(t, http.StatusCreated, c.Post("/api/v1/vessels", body(), IdempotencyKey(strings.Repeat("k", 219)+uuid.NewString())).Status, "255 chars")
	tooLong := c.Post("/api/v1/vessels", body(), IdempotencyKey(strings.Repeat("k", 256)))
	assert.Equal(t, http.StatusBadRequest, tooLong.Status)
	assert.Contains(t, ErrorOf(t, tooLong).Message, "Idempotency-Key")
}

func TestIdempotency_EmptyHeaderMeansNoKey(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)
	body := map[string]any{"name": "x", "weight_kg": 1, "vessel_id": vessel.ID}

	c.Post("/api/v1/cargoes", body, IdempotencyKey(""))
	c.Post("/api/v1/cargoes", body, IdempotencyKey(""))

	assert.Equal(t, 2, cargoCount(t, vessel.ID))
}

func TestIdempotency_ReadOnlyEndpointsIgnoreTheKey(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()
	key := IdempotencyKey(uuid.NewString())

	first := c.Get(CargoPath(cargo.ID), key)
	c.MustChangeStatus(cargo.ID, InTransit)
	second := c.Get(CargoPath(cargo.ID), key)

	assert.NotEqual(t, first.String(), second.String(), "GET always returns fresh state")
	assert.Empty(t, second.Header.Get("Idempotent-Replayed"))
}
