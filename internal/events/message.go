// Package events implements the asynchronous side of cargo status changes:
// the outbox relay that publishes events and the consumer that stores them.
package events

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/drzbraz/deus.ai/internal/domain"
)

// RoutingKeyStatusChanged identifies cargo status change messages on the broker.
const RoutingKeyStatusChanged = "cargo.status_changed"

// StatusChangedMessage is the public wire contract of a status change event.
type StatusChangedMessage struct {
	EventID   uuid.UUID `json:"event_id"`
	CargoID   uuid.UUID `json:"cargo_id"`
	OldStatus string    `json:"old_status"`
	NewStatus string    `json:"new_status"`
	Timestamp time.Time `json:"timestamp"`
}

// EncodeStatusChanged serialises a domain event into its wire format.
func EncodeStatusChanged(event domain.StatusChanged) ([]byte, error) {
	payload, err := json.Marshal(StatusChangedMessage{
		EventID:   event.EventID,
		CargoID:   event.CargoID,
		OldStatus: string(event.OldStatus),
		NewStatus: string(event.NewStatus),
		Timestamp: event.OccurredAt,
	})
	if err != nil {
		return nil, fmt.Errorf("encode status changed event: %w", err)
	}
	return payload, nil
}

// DecodeStatusChanged parses and validates a wire message. Any error means the
// message is malformed and retrying it will never succeed.
func DecodeStatusChanged(payload []byte) (domain.StatusChanged, error) {
	var msg StatusChangedMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return domain.StatusChanged{}, fmt.Errorf("decode status changed event: %w", err)
	}

	oldStatus, oldErr := domain.ParseStatus(msg.OldStatus)
	newStatus, newErr := domain.ParseStatus(msg.NewStatus)
	switch {
	case msg.EventID == uuid.Nil || msg.CargoID == uuid.Nil:
		return domain.StatusChanged{}, fmt.Errorf("decode status changed event: missing event_id or cargo_id")
	case oldErr != nil || newErr != nil:
		return domain.StatusChanged{}, fmt.Errorf("decode status changed event: unknown status %q -> %q", msg.OldStatus, msg.NewStatus)
	case msg.Timestamp.IsZero():
		return domain.StatusChanged{}, fmt.Errorf("decode status changed event: missing timestamp")
	}

	return domain.StatusChanged{
		EventID:    msg.EventID,
		CargoID:    msg.CargoID,
		OldStatus:  oldStatus,
		NewStatus:  newStatus,
		OccurredAt: msg.Timestamp,
	}, nil
}

// OutboxMessage is an event waiting in the outbox to be published.
type OutboxMessage struct {
	ID         uuid.UUID
	RoutingKey string
	Payload    []byte
	// Headers carry the W3C trace context captured when the event was created.
	Headers  map[string]string
	Attempts int
}
