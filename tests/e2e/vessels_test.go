//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	. "github.com/drzbraz/deus.ai/tests/testkit"
)

func TestVessel_CreateReturnsResourceAndLocation(t *testing.T) {
	c := api(t)
	name := UniqueName("Ever Given")

	resp := c.Post("/api/v1/vessels", map[string]any{"name": name, "capacity_kg": 20000, "current_location": "Rotterdam"})

	require.Equal(t, http.StatusCreated, resp.Status, resp.String())
	vessel := Data[Vessel](t, resp)
	assert.NotEmpty(t, vessel.ID)
	assert.Equal(t, name, vessel.Name)
	assert.Equal(t, 20000.0, vessel.CapacityKg)
	assert.Equal(t, "Rotterdam", vessel.CurrentLocation)
	assert.Zero(t, vessel.LoadKg)
	assert.Equal(t, 20000.0, vessel.AvailableCapacityKg)
	assert.Equal(t, vessel.CreatedAt, vessel.UpdatedAt)
	assert.WithinDuration(t, time.Now(), vessel.CreatedAt, time.Minute)

	location := resp.Header.Get("Location")
	assert.Equal(t, VesselPath(vessel.ID), location)
	assert.Equal(t, vessel, c.GetVessel(vessel.ID), "Location must resolve to the created vessel")
}

func TestVessel_InputIsTrimmed(t *testing.T) {
	name := UniqueName("Trimmed")
	resp := api(t).Post("/api/v1/vessels", map[string]any{"name": "  " + name + "  ", "capacity_kg": 1, "current_location": "  Lisbon \t"})

	require.Equal(t, http.StatusCreated, resp.Status)
	vessel := Data[Vessel](t, resp)
	assert.Equal(t, name, vessel.Name)
	assert.Equal(t, "Lisbon", vessel.CurrentLocation)
}

func TestVessel_DuplicateNameIsConflict(t *testing.T) {
	c := api(t)
	name := UniqueName("Twin")
	body := map[string]any{"name": name, "capacity_kg": 1, "current_location": "Lisbon"}
	require.Equal(t, http.StatusCreated, c.Post("/api/v1/vessels", body).Status)

	exact := c.Post("/api/v1/vessels", body)
	padded := c.Post("/api/v1/vessels", map[string]any{"name": " " + name + " ", "capacity_kg": 1, "current_location": "Lisbon"})

	for _, resp := range []Response{exact, padded} {
		assert.Equal(t, http.StatusConflict, resp.Status)
		assert.Equal(t, "already_exists", ErrorOf(t, resp).Code)
	}
}

func TestVessel_NamesAreCaseSensitive(t *testing.T) {
	c := api(t)
	name := UniqueName("case")
	require.Equal(t, http.StatusCreated, c.Post("/api/v1/vessels", map[string]any{"name": name, "capacity_kg": 1, "current_location": "x"}).Status)

	resp := c.Post("/api/v1/vessels", map[string]any{"name": strings.ToUpper(name), "capacity_kg": 1, "current_location": "x"})

	assert.Equal(t, http.StatusCreated, resp.Status)
}

func TestVessel_Validation(t *testing.T) {
	valid := func() map[string]any {
		return map[string]any{"name": UniqueName("v"), "capacity_kg": 100, "current_location": "Lisbon"}
	}
	with := func(field string, value any) map[string]any {
		body := valid()
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
		{"missing name", with("name", nil), "name", "is required"},
		{"blank name", with("name", "   "), "name", "is required"},
		{"name too long", with("name", strings.Repeat("n", 201)), "name", "must be at most 200 characters"},
		{"missing capacity", with("capacity_kg", nil), "capacity_kg", "must be greater than 0"},
		{"zero capacity", with("capacity_kg", 0), "capacity_kg", "must be greater than 0"},
		{"negative capacity", with("capacity_kg", -5), "capacity_kg", "must be greater than 0"},
		{"capacity above maximum", with("capacity_kg", 1_000_000_000.01), "capacity_kg", "must be at most 1000000000"},
		{"missing location", with("current_location", nil), "current_location", "is required"},
		{"blank location", with("current_location", "\t "), "current_location", "is required"},
		{"location too long", with("current_location", strings.Repeat("l", 201)), "current_location", "must be at most 200 characters"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := api(t).Post("/api/v1/vessels", tt.body)

			require.Equal(t, http.StatusUnprocessableEntity, resp.Status, resp.String())
			apiErr := ErrorOf(t, resp)
			assert.Equal(t, "validation_failed", apiErr.Code)
			assert.Equal(t, tt.wantReason, apiErr.Details[tt.wantField])
		})
	}
}

func TestVessel_ValidationReportsEveryFieldAtOnce(t *testing.T) {
	resp := api(t).Post("/api/v1/vessels", map[string]any{})

	require.Equal(t, http.StatusUnprocessableEntity, resp.Status)
	assert.Len(t, ErrorOf(t, resp).Details, 3)
}

func TestVessel_BoundaryValuesAreAccepted(t *testing.T) {
	tests := map[string]map[string]any{
		"name of exactly 200 chars": {"name": strings.Repeat("a", 164) + uuid.NewString(), "capacity_kg": 1, "current_location": "x"},
		"smallest capacity":         {"name": UniqueName("tiny"), "capacity_kg": 0.01, "current_location": "x"},
		"largest capacity":          {"name": UniqueName("huge"), "capacity_kg": 1_000_000_000, "current_location": "x"},
		"location of exactly 200":   {"name": UniqueName("loc"), "capacity_kg": 1, "current_location": strings.Repeat("l", 200)},
		"unicode name and location": {"name": UniqueName("Navio São João 🚢"), "capacity_kg": 1, "current_location": "Ilha da Madeira – Funchal"},
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			resp := api(t).Post("/api/v1/vessels", body)
			require.Equal(t, http.StatusCreated, resp.Status, resp.String())
			assert.Equal(t, body["current_location"], Data[Vessel](t, resp).CurrentLocation)
		})
	}
}

func TestVessel_GetUnknownAndInvalidID(t *testing.T) {
	c := api(t)

	notFound := c.Get(VesselPath(uuid.NewString()))
	assert.Equal(t, http.StatusNotFound, notFound.Status)
	assert.Equal(t, "not_found", ErrorOf(t, notFound).Code)

	for _, id := range []string{"abc", "123", "00000000-0000-0000-0000", "%20"} {
		resp := c.Get(VesselPath(id))
		assert.Equal(t, http.StatusBadRequest, resp.Status, id)
	}
}

func TestVessel_ListPaginationIsStableAndDisjoint(t *testing.T) {
	c := api(t)
	for range 5 {
		c.NewVessel(10)
	}

	all := Data[[]Vessel](t, c.Get("/api/v1/vessels?limit=4&offset=0"))
	first := Data[[]Vessel](t, c.Get("/api/v1/vessels?limit=2&offset=0"))
	second := Data[[]Vessel](t, c.Get("/api/v1/vessels?limit=2&offset=2"))

	require.Len(t, all, 4)
	assert.Equal(t, all[:2], first)
	assert.Equal(t, all[2:], second)
}

func TestVessel_ListMeta(t *testing.T) {
	c := api(t)
	c.NewVessel(10)

	defaults := c.Get("/api/v1/vessels")
	assert.Equal(t, 20, MetaOf(t, defaults).Limit)
	assert.Equal(t, 0, MetaOf(t, defaults).Offset)
	assert.Equal(t, len(Data[[]Vessel](t, defaults)), MetaOf(t, defaults).Count)

	zero := c.Get("/api/v1/vessels?limit=0")
	assert.Equal(t, 20, MetaOf(t, zero).Limit, "limit=0 means default")

	beyond := c.Get("/api/v1/vessels?limit=5&offset=100000000")
	assert.Equal(t, http.StatusOK, beyond.Status)
	assert.Empty(t, Data[[]Vessel](t, beyond))
	assert.JSONEq(t, `[]`, extractData(t, beyond), "empty pages are [] not null")
}

func TestVessel_ListRejectsBadPagination(t *testing.T) {
	tests := map[string]int{
		"limit=abc":        http.StatusBadRequest,
		"offset=1.5":       http.StatusBadRequest,
		"limit=101":        http.StatusUnprocessableEntity,
		"limit=-1":         http.StatusUnprocessableEntity,
		"offset=-1":        http.StatusUnprocessableEntity,
		"limit=1&offset=x": http.StatusBadRequest,
	}
	for query, want := range tests {
		t.Run(query, func(t *testing.T) {
			assert.Equal(t, want, api(t).Get("/api/v1/vessels?"+query).Status)
		})
	}
}

func TestVessel_UpdateLocation(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)

	resp := c.Patch(VesselPath(vessel.ID)+"/location", map[string]any{"current_location": "Singapore"})

	require.Equal(t, http.StatusOK, resp.Status, resp.String())
	updated := Data[Vessel](t, resp)
	assert.Equal(t, "Singapore", updated.CurrentLocation)
	assert.Equal(t, vessel.CreatedAt, updated.CreatedAt)
	assert.True(t, updated.UpdatedAt.After(vessel.UpdatedAt))
	assert.Equal(t, "Singapore", c.GetVessel(vessel.ID).CurrentLocation)
}

func TestVessel_UpdateLocationErrors(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)
	path := VesselPath(vessel.ID) + "/location"

	blank := c.Patch(path, map[string]any{"current_location": "  "})
	assert.Equal(t, http.StatusUnprocessableEntity, blank.Status)
	assert.Contains(t, ErrorOf(t, blank).Details, "current_location")
	assert.Equal(t, "Rotterdam", c.GetVessel(vessel.ID).CurrentLocation, "failed update changes nothing")

	assert.Equal(t, http.StatusNotFound, c.Patch(VesselPath(uuid.NewString())+"/location", map[string]any{"current_location": "x"}).Status)
	assert.Equal(t, http.StatusBadRequest, c.Patch(VesselPath("nope")+"/location", map[string]any{"current_location": "x"}).Status)
	assert.Equal(t, http.StatusBadRequest, c.Patch(path, map[string]any{"location": "x"}).Status, "unknown field")
	assert.Equal(t, http.StatusBadRequest, c.Patch(path, map[string]any{"current_location": "x", "name": "rename"}).Status, "name is immutable")
}

func TestVessel_MovingDoesNotRewriteCargoHistory(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(100)
	cargo := c.NewCargo(vessel.ID, 1)
	before := c.History(cargo.ID)

	c.Patch(VesselPath(vessel.ID)+"/location", map[string]any{"current_location": "Panama"})

	assert.Equal(t, before, c.History(cargo.ID))
}

func TestVessel_LoadTracksCargoLifecycle(t *testing.T) {
	c := api(t)
	vessel := c.NewVessel(1000)
	pending := c.NewCargo(vessel.ID, 100)
	moving := c.NewCargo(vessel.ID, 200)
	delivered := c.NewCargo(vessel.ID, 300)
	c.MustChangeStatus(moving.ID, InTransit)
	c.MustChangeStatus(delivered.ID, InTransit)
	c.MustChangeStatus(delivered.ID, Delivered)
	_ = pending

	got := c.GetVessel(vessel.ID)

	assert.Equal(t, 300.0, got.LoadKg, "pending + in_transit count, delivered does not")
	assert.Equal(t, 700.0, got.AvailableCapacityKg)

	listed := Data[[]Vessel](t, c.Get(fmt.Sprintf("/api/v1/vessels?limit=100&offset=%d", 0)))
	for _, v := range listed {
		if v.ID == vessel.ID {
			assert.Equal(t, 300.0, v.LoadKg, "list and detail agree")
		}
	}
}
