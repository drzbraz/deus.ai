package domain

import "slices"

// Status is the lifecycle state of a cargo.
type Status string

const (
	StatusPending   Status = "pending"
	StatusInTransit Status = "in_transit"
	StatusDelivered Status = "delivered"
)

// allowedTransitions is the single source of truth for the cargo state machine.
// Cargo only moves forward: a delivered cargo can never be "undelivered".
var allowedTransitions = map[Status][]Status{
	StatusPending:   {StatusInTransit},
	StatusInTransit: {StatusDelivered},
	StatusDelivered: {},
}

// ParseStatus converts raw input into a known Status.
func ParseStatus(raw string) (Status, error) {
	status := Status(raw)
	if !status.IsValid() {
		return "", NewValidationError("status", "must be one of: pending, in_transit, delivered")
	}
	return status, nil
}

// IsValid reports whether the status is part of the state machine.
func (s Status) IsValid() bool {
	_, known := allowedTransitions[s]
	return known
}

// CanTransitionTo reports whether moving from s to next is allowed.
func (s Status) CanTransitionTo(next Status) bool {
	return slices.Contains(allowedTransitions[s], next)
}

// IsFinal reports whether no further transitions are possible.
func (s Status) IsFinal() bool {
	return len(allowedTransitions[s]) == 0
}
