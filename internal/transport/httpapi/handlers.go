package httpapi

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/drzbraz/deus.ai/internal/domain"
	"github.com/drzbraz/deus.ai/internal/service"
)

// Ports used by the handlers, declared where they are consumed.

type VesselService interface {
	Create(ctx context.Context, in service.CreateVesselInput) (domain.Vessel, error)
	Get(ctx context.Context, id uuid.UUID) (domain.Vessel, error)
	List(ctx context.Context, page domain.Page) ([]domain.Vessel, error)
	UpdateLocation(ctx context.Context, id uuid.UUID, location string) (domain.Vessel, error)
}

type CargoService interface {
	Create(ctx context.Context, in service.CreateCargoInput) (domain.Cargo, error)
	Get(ctx context.Context, id uuid.UUID) (domain.Cargo, error)
	List(ctx context.Context, filter domain.CargoFilter) ([]domain.Cargo, error)
	UpdateStatus(ctx context.Context, in service.UpdateStatusInput) (domain.Cargo, error)
}

type TrackingService interface {
	RecordMovement(ctx context.Context, in service.RecordMovementInput) (domain.TrackingEntry, error)
	History(ctx context.Context, cargoID uuid.UUID) ([]domain.TrackingEntry, error)
	Events(ctx context.Context, cargoID uuid.UUID) ([]domain.CargoEvent, error)
}

// Handlers are thin: decode and validate the HTTP input, call one service
// method, encode the result. Business rules live in the service and domain.

type vesselHandler struct {
	responder
	svc VesselService
}

func (h vesselHandler) create(w http.ResponseWriter, r *http.Request) {
	var req createVesselRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.error(w, r, err)
		return
	}

	vessel, err := h.svc.Create(r.Context(), service.CreateVesselInput(req))
	if err != nil {
		h.error(w, r, err)
		return
	}

	w.Header().Set("Location", "/api/v1/vessels/"+vessel.ID.String())
	h.ok(w, http.StatusCreated, toVesselResponse(vessel))
}

func (h vesselHandler) get(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "vesselID")
	if err != nil {
		h.error(w, r, err)
		return
	}

	vessel, err := h.svc.Get(r.Context(), id)
	if err != nil {
		h.error(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, toVesselResponse(vessel))
}

func (h vesselHandler) list(w http.ResponseWriter, r *http.Request) {
	page, err := queryPage(r)
	if err != nil {
		h.error(w, r, err)
		return
	}

	vessels, err := h.svc.List(r.Context(), page)
	if err != nil {
		h.error(w, r, err)
		return
	}
	h.page(w, mapSlice(vessels, toVesselResponse), page, len(vessels))
}

func (h vesselHandler) updateLocation(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "vesselID")
	if err != nil {
		h.error(w, r, err)
		return
	}
	var req updateVesselLocationRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.error(w, r, err)
		return
	}

	vessel, err := h.svc.UpdateLocation(r.Context(), id, req.CurrentLocation)
	if err != nil {
		h.error(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, toVesselResponse(vessel))
}

type cargoHandler struct {
	responder
	svc CargoService
}

func (h cargoHandler) create(w http.ResponseWriter, r *http.Request) {
	var req createCargoRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.error(w, r, err)
		return
	}
	vesselID, err := bodyUUID("vessel_id", req.VesselID)
	if err != nil {
		h.error(w, r, err)
		return
	}

	cargo, err := h.svc.Create(r.Context(), service.CreateCargoInput{
		Name: req.Name, Description: req.Description, WeightKg: req.WeightKg, VesselID: vesselID,
	})
	if err != nil {
		h.error(w, r, err)
		return
	}

	w.Header().Set("Location", "/api/v1/cargoes/"+cargo.ID.String())
	w.Header().Set("ETag", etag(cargo.Version))
	h.ok(w, http.StatusCreated, toCargoResponse(cargo))
}

func (h cargoHandler) get(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "cargoID")
	if err != nil {
		h.error(w, r, err)
		return
	}

	cargo, err := h.svc.Get(r.Context(), id)
	if err != nil {
		h.error(w, r, err)
		return
	}

	w.Header().Set("ETag", etag(cargo.Version))
	h.ok(w, http.StatusOK, toCargoResponse(cargo))
}

func (h cargoHandler) list(w http.ResponseWriter, r *http.Request) {
	filter, err := cargoFilterFromQuery(r)
	if err != nil {
		h.error(w, r, err)
		return
	}

	cargoes, err := h.svc.List(r.Context(), filter)
	if err != nil {
		h.error(w, r, err)
		return
	}
	h.page(w, mapSlice(cargoes, toCargoResponse), filter.Page, len(cargoes))
}

func cargoFilterFromQuery(r *http.Request) (domain.CargoFilter, error) {
	page, err := queryPage(r)
	if err != nil {
		return domain.CargoFilter{}, err
	}
	filter := domain.CargoFilter{Page: page}

	if raw := r.URL.Query().Get("status"); raw != "" {
		status, err := domain.ParseStatus(raw)
		if err != nil {
			return domain.CargoFilter{}, err
		}
		filter.Status = &status
	}
	if raw := r.URL.Query().Get("vessel_id"); raw != "" {
		vesselID, err := uuid.Parse(raw)
		if err != nil {
			return domain.CargoFilter{}, badRequest("query parameter vessel_id must be a valid UUID")
		}
		filter.VesselID = &vesselID
	}
	return filter, nil
}

func (h cargoHandler) updateStatus(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "cargoID")
	if err != nil {
		h.error(w, r, err)
		return
	}
	expectedVersion, err := ifMatchVersion(r)
	if err != nil {
		h.error(w, r, err)
		return
	}
	var req updateCargoStatusRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.error(w, r, err)
		return
	}

	cargo, err := h.svc.UpdateStatus(r.Context(), service.UpdateStatusInput{
		CargoID:         id,
		Status:          req.Status,
		Location:        req.Location,
		Note:            req.Note,
		ExpectedVersion: expectedVersion,
	})
	if err != nil {
		h.error(w, r, err)
		return
	}

	w.Header().Set("ETag", etag(cargo.Version))
	h.ok(w, http.StatusOK, toCargoResponse(cargo))
}

type trackingHandler struct {
	responder
	svc TrackingService
}

func (h trackingHandler) recordMovement(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "cargoID")
	if err != nil {
		h.error(w, r, err)
		return
	}
	var req recordMovementRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.error(w, r, err)
		return
	}

	entry, err := h.svc.RecordMovement(r.Context(), service.RecordMovementInput{
		CargoID: id, Location: req.Location, Note: req.Note,
	})
	if err != nil {
		h.error(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, toTrackingEntryResponse(entry))
}

func (h trackingHandler) history(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "cargoID")
	if err != nil {
		h.error(w, r, err)
		return
	}

	entries, err := h.svc.History(r.Context(), id)
	if err != nil {
		h.error(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, mapSlice(entries, toTrackingEntryResponse))
}

func (h trackingHandler) events(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "cargoID")
	if err != nil {
		h.error(w, r, err)
		return
	}

	events, err := h.svc.Events(r.Context(), id)
	if err != nil {
		h.error(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, mapSlice(events, toCargoEventResponse))
}
