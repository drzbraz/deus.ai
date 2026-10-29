// Package service implements the application use cases. It depends only on
// the domain and on the ports (interfaces) declared in this file; adapters
// such as the Postgres repositories implement them.
package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/drzbraz/deus.ai/internal/domain"
)

// TxManager runs fn inside a single database transaction (Unit of Work).
// Repositories called with the ctx passed to fn take part in that transaction.
type TxManager interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

type VesselRepository interface {
	Create(ctx context.Context, vessel domain.Vessel) error
	Get(ctx context.Context, id uuid.UUID) (domain.Vessel, error)
	// GetForUpdate locks the vessel row until the transaction ends, which
	// serialises capacity checks for concurrent cargo creation.
	GetForUpdate(ctx context.Context, id uuid.UUID) (domain.Vessel, error)
	List(ctx context.Context, page domain.Page) ([]domain.Vessel, error)
	Update(ctx context.Context, vessel domain.Vessel) error
}

type CargoRepository interface {
	Create(ctx context.Context, cargo domain.Cargo) error
	Get(ctx context.Context, id uuid.UUID) (domain.Cargo, error)
	// GetForUpdate locks the cargo row so concurrent status changes are serialised.
	GetForUpdate(ctx context.Context, id uuid.UUID) (domain.Cargo, error)
	List(ctx context.Context, filter domain.CargoFilter) ([]domain.Cargo, error)
	Update(ctx context.Context, cargo domain.Cargo) error
}

type TrackingRepository interface {
	Append(ctx context.Context, entry domain.TrackingEntry) error
	ListByCargo(ctx context.Context, cargoID uuid.UUID) ([]domain.TrackingEntry, error)
}

// Outbox stores domain events in the caller's transaction for later publishing.
type Outbox interface {
	Enqueue(ctx context.Context, event domain.StatusChanged) error
}

type CargoEventReader interface {
	ListByCargo(ctx context.Context, cargoID uuid.UUID) ([]domain.CargoEvent, error)
}

// TransitionRecorder receives a business metric for every committed status change.
type TransitionRecorder interface {
	RecordTransition(ctx context.Context, from, to domain.Status)
}

// Repositories bundles the ports so constructors stay short and readable.
type Repositories struct {
	Tx       TxManager
	Vessels  VesselRepository
	Cargoes  CargoRepository
	Tracking TrackingRepository
	Outbox   Outbox
	Events   CargoEventReader
}

// Clock returns the current time. It is injected so tests are deterministic.
type Clock func() time.Time

// SystemClock returns UTC time truncated to microseconds, the precision
// Postgres stores, so values returned to clients match what is persisted.
func SystemClock() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}
