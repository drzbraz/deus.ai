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

func TestCargo_CreateStartsPendingWithVersionAndETag(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(1000)

	resp := c.Post("/api/v1/cargoes", map[string]any{
		"name": "  Coffee beans ", "description": " Arabica ", "weight_kg": 12.5, "vessel_id": vessel.ID,
	})

	require.Equal(t, http.StatusCreated, resp.Status, resp.String())
	cargo := Data[Cargo](t, resp)
	assert.Equal(t, "Coffee beans", cargo.Name)
	assert.Equal(t, "Arabica", cargo.Description)
	assert.Equal(t, 12.5, cargo.WeightKg)
	assert.Equal(t, Pending, cargo.Status)
	assert.Equal(t, vessel.ID, cargo.VesselID)
	assert.Equal(t, 1, cargo.Version)
	assert.Equal(t, `"1"`, resp.Header.Get("ETag"))
	assert.Equal(t, CargoPath(cargo.ID), resp.Header.Get("Location"))
	assert.Equal(t, cargo, c.GetCargo(cargo.ID))
}

func TestCargo_CreationRecordsFirstMovementAtVesselLocation(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)
	c.Patch(VesselPath(vessel.ID)+"/location", map[string]any{"current_location": "Antwerp"})

	cargo := c.NewCargo(vessel.ID, 1)
	history := c.History(cargo.ID)

	require.Len(t, history, 1)
	assert.Equal(t, Pending, history[0].Status)
	assert.Equal(t, "Antwerp", history[0].Location)
	assert.Equal(t, "cargo registered", history[0].Note)
	assert.Equal(t, cargo.CreatedAt, history[0].RecordedAt)
}

func TestCargo_CreationEmitsNoEvent(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()

	c.AssertEventCountStays(cargo.ID, 0)
}

func TestCargo_ClientCannotChooseInitialStatusOrVersion(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)

	for _, extra := range []string{`"status":"delivered"`, `"version":7`, `"id":"` + uuid.NewString() + `"`} {
		body := `{"name":"x","weight_kg":1,"vessel_id":"` + vessel.ID + `",` + extra + `}`
		resp := c.Post("/api/v1/cargoes", body)
		assert.Equal(t, http.StatusBadRequest, resp.Status, extra)
		assert.Contains(t, ErrorOf(t, resp).Message, "unknown field")
	}
}

func TestCargo_Validation(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(1000)
	base := func() map[string]any {
		return map[string]any{"name": "Tea", "weight_kg": 1, "vessel_id": vessel.ID}
	}
	set := func(field string, value any) map[string]any {
		body := base()
		if value == nil {
			delete(body, field)
		} else {
			body[field] = value
		}
		return body
	}

	tests := []struct {
		name       string
		body       map[string]any
		wantField  string
		wantReason string
	}{
		{"missing name", set("name", nil), "name", "is required"},
		{"blank name", set("name", " "), "name", "is required"},
		{"name too long", set("name", strings.Repeat("n", 201)), "name", "must be at most 200 characters"},
		{"description too long", set("description", strings.Repeat("d", 1001)), "description", "must be at most 1000 characters"},
		{"zero weight", set("weight_kg", 0), "weight_kg", "must be greater than 0"},
		{"negative weight", set("weight_kg", -1), "weight_kg", "must be greater than 0"},
		{"weight above maximum", set("weight_kg", 2_000_000_000), "weight_kg", "must be at most 1000000000"},
		{"missing vessel", set("vessel_id", nil), "vessel_id", "is required"},
		{"malformed vessel id", set("vessel_id", "vessel-1"), "vessel_id", "must be a valid UUID"},
		{"unknown vessel", set("vessel_id", uuid.NewString()), "vessel_id", "vessel does not exist"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := c.Post("/api/v1/cargoes", tt.body)

			require.Equal(t, http.StatusUnprocessableEntity, resp.Status, resp.String())
			assert.Equal(t, tt.wantReason, ErrorOf(t, resp).Details[tt.wantField])
		})
	}

	listed := Data[[]Cargo](t, c.Get("/api/v1/cargoes?vessel_id="+vessel.ID))
	assert.Empty(t, listed, "no invalid request may create a cargo")
}

func TestCargo_WrongJSONTypes(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(10)
	for name, body := range map[string]string{
		"weight as string":   `{"name":"x","weight_kg":"10","vessel_id":"` + vessel.ID + `"}`,
		"name as number":     `{"name":42,"weight_kg":1,"vessel_id":"` + vessel.ID + `"}`,
		"vessel id as array": `{"name":"x","weight_kg":1,"vessel_id":["` + vessel.ID + `"]}`,
		"null body":          `null`,
	} {
		t.Run(name, func(t *testing.T) {
			resp := c.Post("/api/v1/cargoes", body)
			assert.Contains(t, []int{http.StatusBadRequest, http.StatusUnprocessableEntity}, resp.Status, resp.String())
		})
	}
}

func TestCargo_CapacityRules(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)

	exact := c.NewCargo(vessel.ID, 100)
	assert.Equal(t, 100.0, c.GetVessel(vessel.ID).LoadKg, "filling exactly to capacity is allowed")

	over := c.Post("/api/v1/cargoes", map[string]any{"name": "x", "weight_kg": 0.01, "vessel_id": vessel.ID})
	require.Equal(t, http.StatusConflict, over.Status)
	assert.Equal(t, "vessel_capacity_exceeded", ErrorOf(t, over).Code)
	assert.Contains(t, ErrorOf(t, over).Message, "0.00 kg free")

	c.MustChangeStatus(exact.ID, InTransit)
	assert.Equal(t, http.StatusConflict, c.Post("/api/v1/cargoes", map[string]any{"name": "x", "weight_kg": 1, "vessel_id": vessel.ID}).Status,
		"in-transit cargo still occupies the vessel")

	c.MustChangeStatus(exact.ID, Delivered)
	c.NewCargo(vessel.ID, 100) // delivery frees the space
}

func TestCargo_SingleCargoHeavierThanVessel(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(50)

	resp := c.Post("/api/v1/cargoes", map[string]any{"name": "x", "weight_kg": 51, "vessel_id": vessel.ID})

	assert.Equal(t, http.StatusConflict, resp.Status)
}

func TestCargo_GetErrors(t *testing.T) {
	c := api(t)
	assert.Equal(t, http.StatusNotFound, c.Get(CargoPath(uuid.NewString())).Status)
	assert.Equal(t, http.StatusBadRequest, c.Get(CargoPath("not-a-uuid")).Status)
}

func TestCargo_ListFilters(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(1000)
	other := c.NewVessel(1000)
	pending := c.NewCargo(vessel.ID, 1)
	moving := c.NewCargo(vessel.ID, 1)
	done := c.NewCargo(vessel.ID, 1)
	elsewhere := c.NewCargo(other.ID, 1)
	c.MustChangeStatus(moving.ID, InTransit)
	c.MustChangeStatus(done.ID, InTransit)
	c.MustChangeStatus(done.ID, Delivered)

	ids := func(query string) []string {
		resp := c.Get("/api/v1/cargoes?" + query)
		require.Equal(t, http.StatusOK, resp.Status, resp.String())
		var out []string
		for _, cargo := range Data[[]Cargo](t, resp) {
			out = append(out, cargo.ID)
		}
		return out
	}

	assert.Equal(t, []string{pending.ID, moving.ID, done.ID}, ids("vessel_id="+vessel.ID), "ordered by creation")
	assert.Equal(t, []string{pending.ID}, ids("vessel_id="+vessel.ID+"&status=pending"))
	assert.Equal(t, []string{moving.ID}, ids("vessel_id="+vessel.ID+"&status=in_transit"))
	assert.Equal(t, []string{done.ID}, ids("vessel_id="+vessel.ID+"&status=delivered"))
	assert.Equal(t, []string{elsewhere.ID}, ids("vessel_id="+other.ID))
	assert.Equal(t, []string{moving.ID}, ids("vessel_id="+vessel.ID+"&limit=1&offset=1"))
	assert.Empty(t, ids("vessel_id="+uuid.NewString()))
}

func TestCargo_ListRejectsBadFilters(t *testing.T) {
	tests := map[string]int{
		"status=lost":    http.StatusUnprocessableEntity,
		"status=PENDING": http.StatusUnprocessableEntity,
		"vessel_id=abc":  http.StatusBadRequest,
		"limit=1000":     http.StatusUnprocessableEntity,
		"offset=minus":   http.StatusBadRequest,
	}
	for query, want := range tests {
		t.Run(query, func(t *testing.T) {
			assert.Equal(t, want, api(t).Get("/api/v1/cargoes?"+query).Status)
		})
	}
}

func TestCargo_UnknownQueryParametersAreIgnored(t *testing.T) {
	assert.Equal(t, http.StatusOK, api(t).Get("/api/v1/cargoes?color=red").Status)
}
