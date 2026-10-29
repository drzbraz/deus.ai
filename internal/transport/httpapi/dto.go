package httpapi

import (
	"time"

	"github.com/google/uuid"

	"github.com/drzbraz/deus.ai/internal/domain"
)

// Request and response DTOs define the public API contract. Keeping them
// separate from domain types lets the API and the domain evolve independently.

type createVesselRequest struct {
	Name            string  `json:"name"`
	CapacityKg      float64 `json:"capacity_kg"`
	CurrentLocation string  `json:"current_location"`
}

type updateVesselLocationRequest struct {
	CurrentLocation string `json:"current_location"`
}

type createCargoRequest struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	WeightKg    float64 `json:"weight_kg"`
	VesselID    string  `json:"vessel_id"`
}

type updateCargoStatusRequest struct {
	Status   string `json:"status"`
	Location string `json:"location"`
	Note     string `json:"note"`
}

type recordMovementRequest struct {
	Location string `json:"location"`
	Note     string `json:"note"`
}

type vesselResponse struct {
	ID                  uuid.UUID `json:"id"`
	Name                string    `json:"name"`
	CapacityKg          float64   `json:"capacity_kg"`
	CurrentLocation     string    `json:"current_location"`
	LoadKg              float64   `json:"load_kg"`
	AvailableCapacityKg float64   `json:"available_capacity_kg"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type cargoResponse struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	WeightKg    float64   `json:"weight_kg"`
	Status      string    `json:"status"`
	VesselID    uuid.UUID `json:"vessel_id"`
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type trackingEntryResponse struct {
	ID         uuid.UUID `json:"id"`
	CargoID    uuid.UUID `json:"cargo_id"`
	Status     string    `json:"status"`
	Location   string    `json:"location"`
	Note       string    `json:"note,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

type cargoEventResponse struct {
	EventID    uuid.UUID `json:"event_id"`
	CargoID    uuid.UUID `json:"cargo_id"`
	OldStatus  string    `json:"old_status"`
	NewStatus  string    `json:"new_status"`
	OccurredAt time.Time `json:"occurred_at"`
	ReceivedAt time.Time `json:"received_at"`
}

func toVesselResponse(v domain.Vessel) vesselResponse {
	return vesselResponse{
		ID:                  v.ID,
		Name:                v.Name,
		CapacityKg:          v.CapacityKg,
		CurrentLocation:     v.CurrentLocation,
		LoadKg:              v.LoadKg,
		AvailableCapacityKg: v.CapacityKg - v.LoadKg,
		CreatedAt:           v.CreatedAt,
		UpdatedAt:           v.UpdatedAt,
	}
}

func toCargoResponse(c domain.Cargo) cargoResponse {
	return cargoResponse{
		ID:          c.ID,
		Name:        c.Name,
		Description: c.Description,
		WeightKg:    c.WeightKg,
		Status:      string(c.Status),
		VesselID:    c.VesselID,
		Version:     c.Version,
		CreatedAt:   c.CreatedAt,
		UpdatedAt:   c.UpdatedAt,
	}
}

func toTrackingEntryResponse(e domain.TrackingEntry) trackingEntryResponse {
	return trackingEntryResponse{
		ID:         e.ID,
		CargoID:    e.CargoID,
		Status:     string(e.Status),
		Location:   e.Location,
		Note:       e.Note,
		RecordedAt: e.RecordedAt,
	}
}

func toCargoEventResponse(e domain.CargoEvent) cargoEventResponse {
	return cargoEventResponse{
		EventID:    e.EventID,
		CargoID:    e.CargoID,
		OldStatus:  string(e.OldStatus),
		NewStatus:  string(e.NewStatus),
		OccurredAt: e.OccurredAt,
		ReceivedAt: e.ReceivedAt,
	}
}

func mapSlice[T, R any](items []T, fn func(T) R) []R {
	out := make([]R, 0, len(items))
	for _, item := range items {
		out = append(out, fn(item))
	}
	return out
}
