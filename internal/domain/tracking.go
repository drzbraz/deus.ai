package domain

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// TrackingEntry is one immutable point in a cargo's movement history.
type TrackingEntry struct {
	ID         uuid.UUID
	CargoID    uuid.UUID
	Status     Status
	Location   string
	Note       string
	RecordedAt time.Time
}

// NewTrackingEntry validates and builds a history entry for a cargo.
func NewTrackingEntry(cargo Cargo, location, note string, now time.Time) (TrackingEntry, error) {
	location = strings.TrimSpace(location)
	note = strings.TrimSpace(note)

	var v validator
	validateLocation(&v, "location", location)
	v.check(len(note) <= MaxNoteLength, "note", fmt.Sprintf("must be at most %d characters", MaxNoteLength))
	if err := v.err(); err != nil {
		return TrackingEntry{}, err
	}

	return TrackingEntry{
		ID:         uuid.Must(uuid.NewV7()),
		CargoID:    cargo.ID,
		Status:     cargo.Status,
		Location:   location,
		Note:       note,
		RecordedAt: now,
	}, nil
}

// StatusChanged is the domain event emitted whenever a cargo changes status.
type StatusChanged struct {
	EventID    uuid.UUID
	CargoID    uuid.UUID
	OldStatus  Status
	NewStatus  Status
	OccurredAt time.Time
}

// CargoEvent is a StatusChanged event as stored by the event consumer.
type CargoEvent struct {
	StatusChanged
	ReceivedAt time.Time
}
