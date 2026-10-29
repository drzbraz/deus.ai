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

func TestStatus_HappyPathThroughTheWholeLifecycle(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()

	inTransit := c.Patch(CargoPath(cargo.ID)+"/status", map[string]any{"status": InTransit, "location": "Suez Canal", "note": "departed"})
	require.Equal(t, http.StatusOK, inTransit.Status, inTransit.String())
	assert.Equal(t, `"2"`, inTransit.Header.Get("ETag"))
	moving := Data[Cargo](t, inTransit)
	assert.Equal(t, InTransit, moving.Status)
	assert.Equal(t, 2, moving.Version)
	assert.True(t, moving.UpdatedAt.After(cargo.UpdatedAt))
	assert.Equal(t, cargo.CreatedAt, moving.CreatedAt)

	delivered := c.Patch(CargoPath(cargo.ID)+"/status", map[string]any{"status": Delivered, "location": "Singapore"})
	require.Equal(t, http.StatusOK, delivered.Status)
	assert.Equal(t, `"3"`, delivered.Header.Get("ETag"))
	assert.Equal(t, Delivered, c.GetCargo(cargo.ID).Status)

	history := c.History(cargo.ID)
	assert.Equal(t, []string{Pending, InTransit, Delivered}, statuses(history))
	assert.Equal(t, []string{"Rotterdam", "Suez Canal", "Singapore"}, locations(history))
	assert.Equal(t, "departed", history[1].Note)

	events := c.WaitForEvents(cargo.ID, 2)
	assert.Equal(t, [][2]string{{Pending, InTransit}, {InTransit, Delivered}},
		[][2]string{{events[0].OldStatus, events[0].NewStatus}, {events[1].OldStatus, events[1].NewStatus}})
}

// The complete 3x3 matrix over HTTP: only two moves are legal.
func TestStatus_TransitionMatrix(t *testing.T) {
	for _, from := range []string{Pending, InTransit, Delivered} {
		for _, to := range []string{Pending, InTransit, Delivered} {
			allowed := (from == Pending && to == InTransit) || (from == InTransit && to == Delivered)
			t.Run(from+"->"+to, func(t *testing.T) {
				c := api(t)
				cargo := c.NewCargoInStatus(from)
				historyBefore := len(c.History(cargo.ID))
				eventsBefore := len(c.WaitForEvents(cargo.ID, historyBefore-1))

				resp := c.ChangeStatus(cargo.ID, to)

				if allowed {
					require.Equal(t, http.StatusOK, resp.Status, resp.String())
					assert.Equal(t, to, c.GetCargo(cargo.ID).Status)
					assert.Len(t, c.History(cargo.ID), historyBefore+1)
					c.WaitForEvents(cargo.ID, eventsBefore+1)
					return
				}

				require.Equal(t, http.StatusConflict, resp.Status, resp.String())
				apiErr := ErrorOf(t, resp)
				assert.Equal(t, "invalid_status_transition", apiErr.Code)
				assert.Contains(t, apiErr.Message, "cannot move from "+from+" to "+to)
				after := c.GetCargo(cargo.ID)
				assert.Equal(t, from, after.Status, "rejected change leaves status untouched")
				assert.Equal(t, cargo.Version, after.Version, "rejected change leaves version untouched")
				assert.Len(t, c.History(cargo.ID), historyBefore, "rejected change writes no history")
				c.AssertEventCountStays(cargo.ID, eventsBefore)
			})
		}
	}
}

func TestStatus_DeliveredIsTerminal(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoInStatus(Delivered)

	for range 3 {
		for _, target := range []string{Pending, InTransit, Delivered} {
			assert.Equal(t, http.StatusConflict, c.ChangeStatus(cargo.ID, target).Status)
		}
	}
	assert.Len(t, c.History(cargo.ID), 3)
}

func TestStatus_InvalidValues(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()
	path := CargoPath(cargo.ID) + "/status"

	for _, value := range []string{"", "PENDING", "In_Transit", "lost", "delivered ", " pending", "cancelled"} {
		resp := c.Patch(path, map[string]any{"status": value})
		assert.Equal(t, http.StatusUnprocessableEntity, resp.Status, "status %q", value)
		assert.Contains(t, ErrorOf(t, resp).Details, "status")
	}

	assert.Equal(t, http.StatusUnprocessableEntity, c.Patch(path, map[string]any{}).Status, "missing status")
	assert.Equal(t, http.StatusBadRequest, c.Patch(path, `{"status":1}`).Status, "wrong type")
	assert.Equal(t, http.StatusBadRequest, c.Patch(path, `{"status":null,"extra":true}`).Status, "unknown field")
	assert.Equal(t, 1, c.GetCargo(cargo.ID).Version)
}

func TestStatus_LocationDefaultsToCurrentVesselLocation(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)
	cargo := c.NewCargo(vessel.ID, 1)
	c.Patch(VesselPath(vessel.ID)+"/location", map[string]any{"current_location": "Port Said"})

	c.MustChangeStatus(cargo.ID, InTransit)

	assert.Equal(t, "Port Said", c.History(cargo.ID)[1].Location)
}

func TestStatus_LocationAndNoteValidationRollsBack(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()
	path := CargoPath(cargo.ID) + "/status"

	tests := map[string]map[string]any{
		"blank location":    {"status": InTransit, "location": "   "},
		"location too long": {"status": InTransit, "location": strings.Repeat("x", 201)},
		"note too long":     {"status": InTransit, "note": strings.Repeat("x", 1001)},
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, http.StatusUnprocessableEntity, c.Patch(path, body).Status)
			assert.Equal(t, Pending, c.GetCargo(cargo.ID).Status, "the status change is rolled back")
			assert.Len(t, c.History(cargo.ID), 1)
		})
	}
	c.AssertEventCountStays(cargo.ID, 0)
}

func TestStatus_UnknownAndInvalidCargo(t *testing.T) {
	c := api(t)
	assert.Equal(t, http.StatusNotFound, c.ChangeStatus(uuid.NewString(), InTransit).Status)
	assert.Equal(t, http.StatusBadRequest, c.ChangeStatus("abc", InTransit).Status)
}

func TestStatus_IfMatch(t *testing.T) {
	t.Run("matching version succeeds", func(t *testing.T) {
		c := api(t)
		cargo := c.NewCargoOnFreshVessel()
		etag := c.Get(CargoPath(cargo.ID)).Header.Get("ETag")

		resp := c.ChangeStatus(cargo.ID, InTransit, IfMatch(etag))

		assert.Equal(t, http.StatusOK, resp.Status)
	})

	t.Run("stale version is rejected and changes nothing", func(t *testing.T) {
		c := api(t)
		cargo := c.NewCargoOnFreshVessel()
		c.MustChangeStatus(cargo.ID, InTransit)

		resp := c.ChangeStatus(cargo.ID, Delivered, IfMatch(`"1"`))

		require.Equal(t, http.StatusPreconditionFailed, resp.Status)
		assert.Equal(t, "version_mismatch", ErrorOf(t, resp).Code)
		assert.Equal(t, InTransit, c.GetCargo(cargo.ID).Status)
		c.WaitForEvents(cargo.ID, 1)
		c.AssertEventCountStays(cargo.ID, 1)
	})

	t.Run("version check runs before the transition check", func(t *testing.T) {
		c := api(t)
		cargo := c.NewCargoOnFreshVessel()

		resp := c.ChangeStatus(cargo.ID, Delivered, IfMatch(`"9"`))

		assert.Equal(t, http.StatusPreconditionFailed, resp.Status)
	})

	t.Run("accepted formats", func(t *testing.T) {
		c := api(t)
		for _, header := range []string{`*`, `W/"1"`, `1`} {
			cargo := c.NewCargoOnFreshVessel()
			assert.Equal(t, http.StatusOK, c.ChangeStatus(cargo.ID, InTransit, IfMatch(header)).Status, header)
		}
	})

	t.Run("malformed header", func(t *testing.T) {
		c := api(t)
		cargo := c.NewCargoOnFreshVessel()
		for _, header := range []string{`"abc"`, `"0"`, `-1`, `"1", "2"`} {
			assert.Equal(t, http.StatusBadRequest, c.ChangeStatus(cargo.ID, InTransit, IfMatch(header)).Status, header)
		}
		assert.Equal(t, Pending, c.GetCargo(cargo.ID).Status)
	})

	t.Run("etag follows every change", func(t *testing.T) {
		c := api(t)
		cargo := c.NewCargoOnFreshVessel()
		for i, status := range []string{InTransit, Delivered} {
			etag := c.Get(CargoPath(cargo.ID)).Header.Get("ETag")
			resp := c.ChangeStatus(cargo.ID, status, IfMatch(etag))
			require.Equal(t, http.StatusOK, resp.Status)
			assert.Equal(t, i+2, Data[Cargo](t, resp).Version)
		}
	})
}
