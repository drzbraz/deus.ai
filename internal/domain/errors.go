package domain

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Sentinel errors shared by every layer. The transport layer maps each one
// to exactly one HTTP status code.
var (
	ErrNotFound            = errors.New("resource not found")
	ErrAlreadyExists       = errors.New("resource already exists")
	ErrInvalidTransition   = errors.New("invalid status transition")
	ErrCapacityExceeded    = errors.New("vessel capacity exceeded")
	ErrCargoDelivered      = errors.New("cargo already delivered")
	ErrVersionMismatch     = errors.New("resource version mismatch")
	ErrIdempotencyKeyReuse = errors.New("idempotency key reused with a different request")
	ErrUnavailable         = errors.New("dependency temporarily unavailable")
	ErrValidation          = errors.New("validation failed")
)

// ValidationError describes which input fields are invalid and why.
type ValidationError struct {
	Fields map[string]string
}

// NewValidationError builds a ValidationError for a single field.
func NewValidationError(field, reason string) *ValidationError {
	return &ValidationError{Fields: map[string]string{field: reason}}
}

func (e *ValidationError) Error() string {
	names := make([]string, 0, len(e.Fields))
	for name := range e.Fields {
		names = append(names, name)
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s: %s", name, e.Fields[name]))
	}
	return fmt.Sprintf("%s: %s", ErrValidation, strings.Join(parts, "; "))
}

// Is lets callers use errors.Is(err, ErrValidation).
func (e *ValidationError) Is(target error) bool {
	return target == ErrValidation
}

// validator accumulates field errors so a client sees every problem at once.
type validator struct {
	fields map[string]string
}

func (v *validator) check(ok bool, field, reason string) {
	if ok {
		return
	}
	if v.fields == nil {
		v.fields = map[string]string{}
	}
	if _, exists := v.fields[field]; !exists {
		v.fields[field] = reason
	}
}

func (v *validator) err() error {
	if len(v.fields) == 0 {
		return nil
	}
	return &ValidationError{Fields: v.fields}
}
