//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	. "github.com/drzbraz/deus.ai/tests/testkit"
)

type writeEndpoint struct {
	name   string
	method string
	path   string
}

// writeEndpoints returns every endpoint that accepts a JSON body.
func writeEndpoints(t *testing.T) []writeEndpoint {
	c := api(t)
	vessel := c.NewVessel(100)
	cargo := c.NewCargo(vessel.ID, 1)
	return []writeEndpoint{
		{"create vessel", http.MethodPost, "/api/v1/vessels"},
		{"move vessel", http.MethodPatch, VesselPath(vessel.ID) + "/location"},
		{"create cargo", http.MethodPost, "/api/v1/cargoes"},
		{"change status", http.MethodPatch, CargoPath(cargo.ID) + "/status"},
		{"record movement", http.MethodPost, CargoPath(cargo.ID) + "/tracking"},
	}
}

func TestContract_MalformedBodiesOnEveryWriteEndpoint(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		opts       []Option
		wantStatus int
		wantCode   string
	}{
		{"empty body", "", nil, http.StatusBadRequest, "bad_request"},
		{"truncated json", `{"name":`, nil, http.StatusBadRequest, "bad_request"},
		{"invalid syntax", `{name: 1}`, nil, http.StatusBadRequest, "bad_request"},
		{"json array", `[{}]`, nil, http.StatusBadRequest, "bad_request"},
		{"json string", `"hello"`, nil, http.StatusBadRequest, "bad_request"},
		{"two objects", `{}{}`, nil, http.StatusBadRequest, "bad_request"},
		{"trailing garbage", `{} x`, nil, http.StatusBadRequest, "bad_request"},
		{"unknown field", `{"unexpected":true}`, nil, http.StatusBadRequest, "bad_request"},
		{"no content type", `{}`, []Option{WithoutContentType()}, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"text/plain", `{}`, []Option{WithContentType("text/plain")}, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"form encoded", `a=b`, []Option{WithContentType("application/x-www-form-urlencoded")}, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"body over 1 MiB", `{"name":"` + strings.Repeat("x", 1<<20) + `"}`, nil, http.StatusRequestEntityTooLarge, "payload_too_large"},
	}

	for _, endpoint := range writeEndpoints(t) {
		for _, tc := range cases {
			t.Run(endpoint.name+"/"+tc.name, func(t *testing.T) {
				var body any = tc.body
				opts := tc.opts
				if tc.body == "" {
					body = nil
					opts = append(opts, WithContentType("application/json"))
				}

				resp := api(t).Do(endpoint.method, endpoint.path, body, opts...)

				require.Equal(t, tc.wantStatus, resp.Status, resp.String())
				assert.Equal(t, tc.wantCode, ErrorOf(t, resp).Code)
			})
		}
	}
}

func TestContract_JSONContentTypeVariantsAreAccepted(t *testing.T) {
	for _, contentType := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON"} {
		resp := api(t).Post("/api/v1/vessels",
			map[string]any{"name": UniqueName("ct"), "capacity_kg": 1, "current_location": "x"}, WithContentType(contentType))
		assert.Equal(t, http.StatusCreated, resp.Status, contentType)
	}
}

func TestContract_InvalidPathIDsAreBadRequests(t *testing.T) {
	c := api(t)
	for _, path := range []string{
		"/api/v1/vessels/x", "/api/v1/cargoes/x", "/api/v1/cargoes/x/tracking", "/api/v1/cargoes/x/events",
	} {
		resp := c.Get(path)
		assert.Equal(t, http.StatusBadRequest, resp.Status, path)
		assert.Equal(t, "bad_request", ErrorOf(t, resp).Code)
	}
}

func TestContract_UnknownRoutesAndMethods(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()

	notFound := []string{"/", "/api", "/api/v2/cargoes", "/api/v1/ships", "/api/v1/cargoes/" + cargo.ID + "/history"}
	for _, path := range notFound {
		resp := c.Get(path)
		assert.Equal(t, http.StatusNotFound, resp.Status, path)
		assert.Equal(t, "route_not_found", ErrorOf(t, resp).Code)
	}

	// No endpoint deletes or overwrites data: history is append-only.
	notAllowed := []struct{ method, path string }{
		{http.MethodDelete, CargoPath(cargo.ID)},
		{http.MethodPut, CargoPath(cargo.ID)},
		{http.MethodDelete, CargoPath(cargo.ID) + "/tracking"},
		{http.MethodDelete, CargoPath(cargo.ID) + "/events"},
		{http.MethodPost, CargoPath(cargo.ID) + "/events"},
		{http.MethodDelete, VesselPath(cargo.VesselID)},
		{http.MethodPost, "/healthz"},
	}
	for _, tc := range notAllowed {
		resp := c.Do(tc.method, tc.path, nil)
		assert.Equal(t, http.StatusMethodNotAllowed, resp.Status, tc.method+" "+tc.path)
		assert.Equal(t, "method_not_allowed", ErrorOf(t, resp).Code)
	}
}

func TestContract_TrailingSlashIsAccepted(t *testing.T) {
	c := api(t)
	assert.Equal(t, http.StatusOK, c.Get("/api/v1/vessels/").Status)
	assert.Equal(t, http.StatusOK, c.Get("/api/v1/cargoes/").Status)
}

func TestContract_EveryResponseIsJSONWithAConsistentEnvelope(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()
	responses := []Response{
		c.Get(CargoPath(cargo.ID)),
		c.Get("/api/v1/cargoes"),
		c.Get(CargoPath(uuid.NewString())),
		c.Get("/nope"),
		c.ChangeStatus(cargo.ID, Delivered),
		c.Post("/api/v1/cargoes", `{`),
		c.Post("/api/v1/cargoes", map[string]any{}),
	}

	for _, r := range responses {
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		var envelope map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(r.Body, &envelope), r.String())
		if r.Status < 400 {
			assert.Contains(t, envelope, "data")
			assert.NotContains(t, envelope, "error")
			continue
		}
		assert.NotContains(t, envelope, "data")
		apiErr := ErrorOf(t, r)
		assert.NotEmpty(t, apiErr.Code)
		assert.NotEmpty(t, apiErr.Message)
	}
}

func TestContract_TimestampsAreUTC(t *testing.T) {
	c := api(t)
	cargo := c.NewCargoOnFreshVessel()
	c.MustChangeStatus(cargo.ID, InTransit)
	c.WaitForEvents(cargo.ID, 1)

	for _, path := range []string{CargoPath(cargo.ID), CargoPath(cargo.ID) + "/tracking", CargoPath(cargo.ID) + "/events", VesselPath(cargo.VesselID)} {
		body := c.Get(path).String()
		assert.NotContains(t, body, "+0", path)
		assert.Regexp(t, `"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z"`, body, path)
	}
}

func TestContract_ErrorsNeverLeakInternals(t *testing.T) {
	c := api(t)
	responses := []Response{
		c.Get(CargoPath(uuid.NewString())),
		c.Post("/api/v1/cargoes", map[string]any{"name": "x", "weight_kg": 1, "vessel_id": uuid.NewString()}),
		c.Post("/api/v1/vessels", map[string]any{"name": "'; DROP TABLE vessels; --", "capacity_kg": -1, "current_location": "x"}),
	}
	for _, r := range responses {
		lower := strings.ToLower(r.String())
		for _, leak := range []string{"sql", "pgx", "postgres", "select ", "insert ", "constraint", "goroutine", ".go:"} {
			assert.NotContains(t, lower, leak)
		}
	}
}

func TestContract_SQLInjectionIsStoredAsPlainText(t *testing.T) {
	c := api(t)
	name := UniqueName("Robert'); DROP TABLE cargoes;--")

	resp := c.Post("/api/v1/vessels", map[string]any{"name": name, "capacity_kg": 1, "current_location": `" OR 1=1 --`})

	require.Equal(t, http.StatusCreated, resp.Status)
	assert.Equal(t, name, c.GetVessel(Data[Vessel](t, resp).ID).Name)
	assert.Equal(t, http.StatusOK, c.Get("/api/v1/cargoes?status=pending").Status, "tables are intact")
}

func TestContract_HealthEndpoints(t *testing.T) {
	c := api(t)

	live := c.Get("/healthz")
	assert.Equal(t, http.StatusOK, live.Status)
	assert.JSONEq(t, `{"status":"ok"}`, live.String())

	ready := c.Get("/readyz")
	assert.Equal(t, http.StatusOK, ready.Status)
	assert.JSONEq(t, `{"status":"ready","checks":{"postgres":"up","rabbitmq":"up"}}`, ready.String())
}
