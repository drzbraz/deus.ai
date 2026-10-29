package domain

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Cargo is a shipment of goods that belongs to a vessel.
type Cargo struct {
	ID          uuid.UUID
	Name        string
	Description string
	WeightKg    float64
	Status      Status
	VesselID    uuid.UUID
	// Version increases on every change and backs optimistic concurrency (ETag/If-Match).
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewCargoParams groups the client-provided fields of a new cargo.
type NewCargoParams struct {
	Name        string
	Description string
	WeightKg    float64
	VesselID    uuid.UUID
}

// NewCargo validates the input and returns a pending cargo. New cargo always
// starts as pending; clients cannot choose the initial status.
func NewCargo(p NewCargoParams, now time.Time) (Cargo, error) {
	name := strings.TrimSpace(p.Name)
	description := strings.TrimSpace(p.Description)

	var v validator
	validateName(&v, "name", name)
	v.check(len(description) <= MaxNoteLength, "description", fmt.Sprintf("must be at most %d characters", MaxNoteLength))
	validateWeight(&v, "weight_kg", p.WeightKg)
	v.check(p.VesselID != uuid.Nil, "vessel_id", "is required")
	if err := v.err(); err != nil {
		return Cargo{}, err
	}

	return Cargo{
		ID:          uuid.Must(uuid.NewV7()),
		Name:        name,
		Description: description,
		WeightKg:    p.WeightKg,
		Status:      StatusPending,
		VesselID:    p.VesselID,
		Version:     1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// ChangeStatus moves the cargo through the state machine and returns the
// domain event describing what happened.
func (c *Cargo) ChangeStatus(next Status, now time.Time) (StatusChanged, error) {
	if !c.Status.CanTransitionTo(next) {
		return StatusChanged{}, fmt.Errorf("%w: cannot move from %s to %s", ErrInvalidTransition, c.Status, next)
	}

	event := StatusChanged{
		EventID:    uuid.Must(uuid.NewV7()),
		CargoID:    c.ID,
		OldStatus:  c.Status,
		NewStatus:  next,
		OccurredAt: now,
	}

	c.Status = next
	c.Version++
	c.UpdatedAt = now
	return event, nil
}
