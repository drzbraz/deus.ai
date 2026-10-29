package domain

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	MaxNameLength     = 200
	MaxLocationLength = 200
	MaxNoteLength     = 1000
	MaxWeightKg       = 1_000_000_000
)

// Vessel is a ship that transports cargo.
type Vessel struct {
	ID              uuid.UUID
	Name            string
	CapacityKg      float64
	CurrentLocation string
	// LoadKg is the total weight of cargo on board that is not yet delivered.
	// It is derived on read and never stored.
	LoadKg    float64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewVessel validates the input and returns a vessel ready to be persisted.
func NewVessel(name string, capacityKg float64, location string, now time.Time) (Vessel, error) {
	name = strings.TrimSpace(name)
	location = strings.TrimSpace(location)

	var v validator
	validateName(&v, "name", name)
	validateWeight(&v, "capacity_kg", capacityKg)
	validateLocation(&v, "current_location", location)
	if err := v.err(); err != nil {
		return Vessel{}, err
	}

	return Vessel{
		ID:              uuid.Must(uuid.NewV7()),
		Name:            name,
		CapacityKg:      capacityKg,
		CurrentLocation: location,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

// MoveTo changes the vessel's current location.
func (v *Vessel) MoveTo(location string, now time.Time) error {
	location = strings.TrimSpace(location)

	var val validator
	validateLocation(&val, "current_location", location)
	if err := val.err(); err != nil {
		return err
	}

	v.CurrentLocation = location
	v.UpdatedAt = now
	return nil
}

// CanLoad reports whether extra weight fits on board given the current load.
func (v Vessel) CanLoad(currentLoadKg, extraKg float64) bool {
	return currentLoadKg+extraKg <= v.CapacityKg
}

func validateName(v *validator, field, name string) {
	v.check(name != "", field, "is required")
	v.check(len(name) <= MaxNameLength, field, fmt.Sprintf("must be at most %d characters", MaxNameLength))
}

func validateLocation(v *validator, field, location string) {
	v.check(location != "", field, "is required")
	v.check(len(location) <= MaxLocationLength, field, fmt.Sprintf("must be at most %d characters", MaxLocationLength))
}

func validateWeight(v *validator, field string, kg float64) {
	v.check(kg > 0, field, "must be greater than 0")
	v.check(kg <= MaxWeightKg, field, fmt.Sprintf("must be at most %d", MaxWeightKg))
}
