package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drzbraz/deus.ai/internal/domain"
	"github.com/drzbraz/deus.ai/internal/service"
)

type envelope struct {
	Data  json.RawMessage `json:"data"`
	Meta  map[string]int  `json:"meta"`
	Error *struct {
		Code    string            `json:"code"`
		Message string            `json:"message"`
		Details map[string]string `json:"details"`
	} `json:"error"`
}

func decode(t *testing.T, body []byte) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(body, &env), string(body))
	return env
}

func TestCreateVessel_Created(t *testing.T) {
	s := newTestServer()
	s.vessels.create = func(in service.CreateVesselInput) (domain.Vessel, error) {
		assert.Equal(t, service.CreateVesselInput{Name: "Ever Given", CapacityKg: 1000, CurrentLocation: "Rotterdam"}, in)
		return sampleVessel(), nil
	}

	rec := s.do(t, http.MethodPost, "/api/v1/vessels", `{"name":"Ever Given","capacity_kg":1000,"current_location":"Rotterdam"}`)

	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "/api/v1/vessels/"+vesselID.String(), rec.Header().Get("Location"))
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"data":{
		"id":"`+vesselID.String()+`","name":"Ever Given","capacity_kg":1000,"current_location":"Rotterdam",
		"load_kg":250,"available_capacity_kg":750,
		"created_at":"2026-10-29T12:00:00Z","updated_at":"2026-10-29T12:00:00Z"}}`, rec.Body.String())
}

// Regression: every malformed request is rejected before reaching a service.
func TestMalformedRequests(t *testing.T) {
	s := newTestServer()
	s.vessels.create = func(service.CreateVesselInput) (domain.Vessel, error) {
		t.Fatal("service must not be called for malformed input")
		return domain.Vessel{}, nil
	}

	tests := []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
		wantCode    string
	}{
		{"empty body", "", "application/json", http.StatusBadRequest, "bad_request"},
		{"malformed json", `{"name":`, "application/json", http.StatusBadRequest, "bad_request"},
		{"syntax error", `{"name" "x"}`, "application/json", http.StatusBadRequest, "bad_request"},
		{"unknown field", `{"name":"x","color":"red"}`, "application/json", http.StatusBadRequest, "bad_request"},
		{"wrong type", `{"capacity_kg":"heavy"}`, "application/json", http.StatusBadRequest, "bad_request"},
		{"trailing data", `{"name":"x"}{"name":"y"}`, "application/json", http.StatusBadRequest, "bad_request"},
		{"not an object", `[1,2]`, "application/json", http.StatusBadRequest, "bad_request"},
		{"missing content type", `{"name":"x"}`, "", http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"wrong content type", `{"name":"x"}`, "text/plain", http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"too large", `{"name":"` + strings.Repeat("x", 2<<20) + `"}`, "application/json", http.StatusRequestEntityTooLarge, "payload_too_large"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newRequest(http.MethodPost, "/api/v1/vessels", tt.body)
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			rec := serve(s.handler(), req)

			assert.Equal(t, tt.wantStatus, rec.Code, rec.Body.String())
			assert.Equal(t, tt.wantCode, decode(t, rec.Body.Bytes()).Error.Code)
		})
	}
}

func TestCreateVessel_JSONCharsetIsAccepted(t *testing.T) {
	s := newTestServer()
	s.vessels.create = func(service.CreateVesselInput) (domain.Vessel, error) { return sampleVessel(), nil }

	req := newRequest(http.MethodPost, "/api/v1/vessels", `{"name":"x"}`)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	assert.Equal(t, http.StatusCreated, serve(s.handler(), req).Code)
}

// Regression: each domain error maps to exactly one documented status code.
func TestErrorMapping(t *testing.T) {
	tests := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{domain.NewValidationError("name", "is required"), http.StatusUnprocessableEntity, "validation_failed"},
		{domain.ErrNotFound, http.StatusNotFound, "not_found"},
		{domain.ErrAlreadyExists, http.StatusConflict, "already_exists"},
		{fmt.Errorf("x: %w", domain.ErrInvalidTransition), http.StatusConflict, "invalid_status_transition"},
		{domain.ErrCapacityExceeded, http.StatusConflict, "vessel_capacity_exceeded"},
		{domain.ErrCargoDelivered, http.StatusConflict, "cargo_already_delivered"},
		{domain.ErrVersionMismatch, http.StatusPreconditionFailed, "version_mismatch"},
		{domain.ErrUnavailable, http.StatusServiceUnavailable, "service_unavailable"},
		{errors.New("pq: relation does not exist"), http.StatusInternalServerError, "internal_error"},
	}

	for _, tt := range tests {
		t.Run(tt.wantCode, func(t *testing.T) {
			s := newTestServer()
			s.vessels.create = func(service.CreateVesselInput) (domain.Vessel, error) { return domain.Vessel{}, tt.err }

			rec := s.do(t, http.MethodPost, "/api/v1/vessels", `{"name":"x"}`)

			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.Equal(t, tt.wantCode, decode(t, rec.Body.Bytes()).Error.Code)
		})
	}
}

func TestErrors_ValidationDetailsAndNoInternalLeaks(t *testing.T) {
	s := newTestServer()
	s.vessels.create = func(service.CreateVesselInput) (domain.Vessel, error) {
		return domain.Vessel{}, &domain.ValidationError{Fields: map[string]string{"name": "is required", "capacity_kg": "must be greater than 0"}}
	}
	rec := s.do(t, http.MethodPost, "/api/v1/vessels", `{}`)
	assert.Equal(t, map[string]string{"name": "is required", "capacity_kg": "must be greater than 0"}, decode(t, rec.Body.Bytes()).Error.Details)

	s.vessels.create = func(service.CreateVesselInput) (domain.Vessel, error) {
		return domain.Vessel{}, errors.New(`insert vessel: ERROR: syntax error at "SELECT"`)
	}
	rec = s.do(t, http.MethodPost, "/api/v1/vessels", `{}`)
	assert.NotContains(t, rec.Body.String(), "SELECT")
	assert.Equal(t, "internal server error", decode(t, rec.Body.Bytes()).Error.Message)
}

func TestUnavailable_SetsRetryAfter(t *testing.T) {
	s := newTestServer()
	s.vessels.get = func(uuid.UUID) (domain.Vessel, error) { return domain.Vessel{}, domain.ErrUnavailable }

	rec := s.do(t, http.MethodGet, "/api/v1/vessels/"+vesselID.String(), "")

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "5", rec.Header().Get("Retry-After"))
}

func TestGetVessel(t *testing.T) {
	s := newTestServer()
	s.vessels.get = func(id uuid.UUID) (domain.Vessel, error) {
		assert.Equal(t, vesselID, id)
		return sampleVessel(), nil
	}

	rec := s.do(t, http.MethodGet, "/api/v1/vessels/"+vesselID.String(), "")
	assert.Equal(t, http.StatusOK, rec.Code)

	rec = s.do(t, http.MethodGet, "/api/v1/vessels/not-a-uuid", "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, decode(t, rec.Body.Bytes()).Error.Message, "vesselID must be a valid UUID")
}

func TestListVessels_Pagination(t *testing.T) {
	s := newTestServer()
	s.vessels.list = func(p domain.Page) ([]domain.Vessel, error) {
		assert.Equal(t, domain.Page{Limit: 5, Offset: 10}, p)
		return []domain.Vessel{sampleVessel()}, nil
	}

	rec := s.do(t, http.MethodGet, "/api/v1/vessels?limit=5&offset=10", "")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, map[string]int{"limit": 5, "offset": 10, "count": 1}, decode(t, rec.Body.Bytes()).Meta)

	assert.Equal(t, http.StatusBadRequest, s.do(t, http.MethodGet, "/api/v1/vessels?limit=ten", "").Code)
	assert.Equal(t, http.StatusBadRequest, s.do(t, http.MethodGet, "/api/v1/vessels?offset=x", "").Code)
	assert.Equal(t, http.StatusUnprocessableEntity, s.do(t, http.MethodGet, "/api/v1/vessels?limit=1000", "").Code)

	s.vessels.list = func(domain.Page) ([]domain.Vessel, error) { return nil, domain.ErrUnavailable }
	assert.Equal(t, http.StatusServiceUnavailable, s.do(t, http.MethodGet, "/api/v1/vessels", "").Code)
}

func TestListVessels_EmptyIsAnArrayNotNull(t *testing.T) {
	s := newTestServer()
	s.vessels.list = func(domain.Page) ([]domain.Vessel, error) { return nil, nil }

	rec := s.do(t, http.MethodGet, "/api/v1/vessels", "")

	assert.JSONEq(t, `[]`, string(decode(t, rec.Body.Bytes()).Data))
}

func TestUpdateVesselLocation(t *testing.T) {
	s := newTestServer()
	s.vessels.updateLocation = func(id uuid.UUID, loc string) (domain.Vessel, error) {
		assert.Equal(t, "Singapore", loc)
		return sampleVessel(), nil
	}

	path := "/api/v1/vessels/" + vesselID.String() + "/location"
	assert.Equal(t, http.StatusOK, s.do(t, http.MethodPatch, path, `{"current_location":"Singapore"}`).Code)
	assert.Equal(t, http.StatusBadRequest, s.do(t, http.MethodPatch, "/api/v1/vessels/x/location", `{}`).Code)
	assert.Equal(t, http.StatusBadRequest, s.do(t, http.MethodPatch, path, `{`).Code)

	s.vessels.updateLocation = func(uuid.UUID, string) (domain.Vessel, error) { return domain.Vessel{}, domain.ErrNotFound }
	assert.Equal(t, http.StatusNotFound, s.do(t, http.MethodPatch, path, `{"current_location":"x"}`).Code)
}

func TestCreateCargo(t *testing.T) {
	s := newTestServer()
	s.cargoes.create = func(in service.CreateCargoInput) (domain.Cargo, error) {
		assert.Equal(t, vesselID, in.VesselID)
		assert.Equal(t, 10.0, in.WeightKg)
		return sampleCargo(), nil
	}

	rec := s.do(t, http.MethodPost, "/api/v1/cargoes", `{"name":"Coffee","weight_kg":10,"vessel_id":"`+vesselID.String()+`"}`)

	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, `"1"`, rec.Header().Get("ETag"))
	assert.Equal(t, "/api/v1/cargoes/"+cargoID.String(), rec.Header().Get("Location"))

	var cargo map[string]any
	require.NoError(t, json.Unmarshal(decode(t, rec.Body.Bytes()).Data, &cargo))
	assert.Equal(t, "pending", cargo["status"])
}

func TestCreateCargo_InvalidVesselIDIsValidationError(t *testing.T) {
	s := newTestServer()
	for body, want := range map[string]string{
		`{"name":"x","weight_kg":1}`:                    "is required",
		`{"name":"x","weight_kg":1,"vessel_id":"nope"}`: "must be a valid UUID",
	} {
		rec := s.do(t, http.MethodPost, "/api/v1/cargoes", body)

		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		assert.Equal(t, want, decode(t, rec.Body.Bytes()).Error.Details["vessel_id"])
	}

	assert.Equal(t, http.StatusBadRequest, s.do(t, http.MethodPost, "/api/v1/cargoes", `nope`).Code)
	s.cargoes.create = func(service.CreateCargoInput) (domain.Cargo, error) {
		return domain.Cargo{}, domain.ErrCapacityExceeded
	}
	rec := s.do(t, http.MethodPost, "/api/v1/cargoes", `{"name":"x","weight_kg":1,"vessel_id":"`+vesselID.String()+`"}`)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestGetCargo_ReturnsETag(t *testing.T) {
	s := newTestServer()
	s.cargoes.get = func(_ context.Context, id uuid.UUID) (domain.Cargo, error) {
		cargo := sampleCargo()
		cargo.Version = 3
		return cargo, nil
	}

	rec := s.do(t, http.MethodGet, "/api/v1/cargoes/"+cargoID.String(), "")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, `"3"`, rec.Header().Get("ETag"))
	assert.Equal(t, http.StatusBadRequest, s.do(t, http.MethodGet, "/api/v1/cargoes/123", "").Code)

	s.cargoes.get = func(context.Context, uuid.UUID) (domain.Cargo, error) { return domain.Cargo{}, domain.ErrNotFound }
	assert.Equal(t, http.StatusNotFound, s.do(t, http.MethodGet, "/api/v1/cargoes/"+cargoID.String(), "").Code)
}

func TestListCargoes_Filters(t *testing.T) {
	s := newTestServer()
	s.cargoes.list = func(f domain.CargoFilter) ([]domain.Cargo, error) {
		require.NotNil(t, f.Status)
		require.NotNil(t, f.VesselID)
		assert.Equal(t, domain.StatusInTransit, *f.Status)
		assert.Equal(t, vesselID, *f.VesselID)
		assert.Equal(t, domain.DefaultPageLimit, f.Page.Limit)
		return []domain.Cargo{sampleCargo()}, nil
	}

	rec := s.do(t, http.MethodGet, "/api/v1/cargoes?status=in_transit&vessel_id="+vesselID.String(), "")
	assert.Equal(t, http.StatusOK, rec.Code)

	assert.Equal(t, http.StatusUnprocessableEntity, s.do(t, http.MethodGet, "/api/v1/cargoes?status=lost", "").Code)
	assert.Equal(t, http.StatusBadRequest, s.do(t, http.MethodGet, "/api/v1/cargoes?vessel_id=abc", "").Code)
	assert.Equal(t, http.StatusBadRequest, s.do(t, http.MethodGet, "/api/v1/cargoes?limit=abc", "").Code)

	s.cargoes.list = func(domain.CargoFilter) ([]domain.Cargo, error) { return nil, errors.New("boom") }
	assert.Equal(t, http.StatusInternalServerError, s.do(t, http.MethodGet, "/api/v1/cargoes", "").Code)
}

func TestUpdateCargoStatus(t *testing.T) {
	s := newTestServer()
	s.cargoes.updateStatus = func(in service.UpdateStatusInput) (domain.Cargo, error) {
		assert.Equal(t, service.UpdateStatusInput{CargoID: cargoID, Status: "in_transit", Location: "Suez", Note: "left port", ExpectedVersion: 1}, in)
		cargo := sampleCargo()
		cargo.Status, cargo.Version = domain.StatusInTransit, 2
		return cargo, nil
	}

	rec := s.do(t, http.MethodPatch, "/api/v1/cargoes/"+cargoID.String()+"/status",
		`{"status":"in_transit","location":"Suez","note":"left port"}`, "If-Match", `"1"`)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, `"2"`, rec.Header().Get("ETag"))
}

func TestUpdateCargoStatus_IfMatchParsing(t *testing.T) {
	s := newTestServer()
	var gotVersion int
	s.cargoes.updateStatus = func(in service.UpdateStatusInput) (domain.Cargo, error) {
		gotVersion = in.ExpectedVersion
		return sampleCargo(), nil
	}
	path := "/api/v1/cargoes/" + cargoID.String() + "/status"

	for header, want := range map[string]int{`W/"4"`: 4, `*`: 0, `7`: 7} {
		rec := s.do(t, http.MethodPatch, path, `{"status":"in_transit"}`, "If-Match", header)
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, want, gotVersion, header)
	}

	for _, header := range []string{`"abc"`, `"0"`} {
		assert.Equal(t, http.StatusBadRequest, s.do(t, http.MethodPatch, path, `{"status":"x"}`, "If-Match", header).Code)
	}
}

func TestUpdateCargoStatus_Errors(t *testing.T) {
	s := newTestServer()
	path := "/api/v1/cargoes/" + cargoID.String() + "/status"

	assert.Equal(t, http.StatusBadRequest, s.do(t, http.MethodPatch, "/api/v1/cargoes/x/status", `{}`).Code)
	assert.Equal(t, http.StatusBadRequest, s.do(t, http.MethodPatch, path, `{"status":1}`).Code)

	s.cargoes.updateStatus = func(service.UpdateStatusInput) (domain.Cargo, error) {
		return domain.Cargo{}, fmt.Errorf("%w: cannot move from pending to delivered", domain.ErrInvalidTransition)
	}
	rec := s.do(t, http.MethodPatch, path, `{"status":"delivered"}`)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, decode(t, rec.Body.Bytes()).Error.Message, "cannot move from pending to delivered")

	s.cargoes.updateStatus = func(service.UpdateStatusInput) (domain.Cargo, error) {
		return domain.Cargo{}, domain.ErrVersionMismatch
	}
	assert.Equal(t, http.StatusPreconditionFailed, s.do(t, http.MethodPatch, path, `{"status":"in_transit"}`, "If-Match", `"1"`).Code)
}

func TestTrackingEndpoints(t *testing.T) {
	s := newTestServer()
	entry := domain.TrackingEntry{ID: uuid.New(), CargoID: cargoID, Status: domain.StatusPending, Location: "Lisbon", RecordedAt: now}
	s.tracking.record = func(in service.RecordMovementInput) (domain.TrackingEntry, error) {
		assert.Equal(t, "Lisbon", in.Location)
		return entry, nil
	}
	s.tracking.history = func(uuid.UUID) ([]domain.TrackingEntry, error) { return []domain.TrackingEntry{entry}, nil }
	s.tracking.events = func(uuid.UUID) ([]domain.CargoEvent, error) {
		return []domain.CargoEvent{{StatusChanged: domain.StatusChanged{CargoID: cargoID, OldStatus: "pending", NewStatus: "in_transit"}}}, nil
	}
	base := "/api/v1/cargoes/" + cargoID.String()

	assert.Equal(t, http.StatusCreated, s.do(t, http.MethodPost, base+"/tracking", `{"location":"Lisbon"}`).Code)

	rec := s.do(t, http.MethodGet, base+"/tracking", "")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"location":"Lisbon"`)

	rec = s.do(t, http.MethodGet, base+"/events", "")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"new_status":"in_transit"`)
}

func TestTrackingEndpoints_Errors(t *testing.T) {
	s := newTestServer()
	s.tracking.record = func(service.RecordMovementInput) (domain.TrackingEntry, error) {
		return domain.TrackingEntry{}, domain.ErrCargoDelivered
	}
	s.tracking.history = func(uuid.UUID) ([]domain.TrackingEntry, error) { return nil, domain.ErrNotFound }
	s.tracking.events = func(uuid.UUID) ([]domain.CargoEvent, error) { return nil, domain.ErrNotFound }
	base := "/api/v1/cargoes/" + cargoID.String()

	assert.Equal(t, http.StatusConflict, s.do(t, http.MethodPost, base+"/tracking", `{"location":"x"}`).Code)
	assert.Equal(t, http.StatusBadRequest, s.do(t, http.MethodPost, base+"/tracking", `{"x":1}`).Code)
	assert.Equal(t, http.StatusNotFound, s.do(t, http.MethodGet, base+"/tracking", "").Code)
	assert.Equal(t, http.StatusNotFound, s.do(t, http.MethodGet, base+"/events", "").Code)
	for _, path := range []string{"/api/v1/cargoes/x/tracking", "/api/v1/cargoes/x/events"} {
		assert.Equal(t, http.StatusBadRequest, s.do(t, http.MethodGet, path, "").Code)
	}
	assert.Equal(t, http.StatusBadRequest, s.do(t, http.MethodPost, "/api/v1/cargoes/x/tracking", `{}`).Code)
}
