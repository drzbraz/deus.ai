package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/drzbraz/deus.ai/internal/domain"
)

// TrackingService exposes the audit trail of a cargo: its movement history
// and the status events stored by the event consumer.
type TrackingService struct {
	repos Repositories
	now   Clock
}

func NewTrackingService(repos Repositories, clock Clock) *TrackingService {
	return &TrackingService{repos: repos, now: clock}
}

type RecordMovementInput struct {
	CargoID  uuid.UUID
	Location string
	Note     string
}

// RecordMovement appends a location update without changing the status.
func (s *TrackingService) RecordMovement(ctx context.Context, in RecordMovementInput) (domain.TrackingEntry, error) {
	var entry domain.TrackingEntry

	err := s.repos.Tx.WithinTx(ctx, func(ctx context.Context) error {
		// Locking the cargo prevents a movement being recorded concurrently
		// with the delivery that should have closed the history.
		cargo, err := s.repos.Cargoes.GetForUpdate(ctx, in.CargoID)
		if err != nil {
			return err
		}
		if cargo.Status.IsFinal() {
			return fmt.Errorf("%w: cargo %s accepts no further movements", domain.ErrCargoDelivered, cargo.ID)
		}

		entry, err = domain.NewTrackingEntry(cargo, in.Location, in.Note, s.now())
		if err != nil {
			return err
		}
		return s.repos.Tracking.Append(ctx, entry)
	})
	if err != nil {
		return domain.TrackingEntry{}, fmt.Errorf("record movement for cargo %s: %w", in.CargoID, err)
	}
	return entry, nil
}

// History returns every tracking entry of a cargo, oldest first.
func (s *TrackingService) History(ctx context.Context, cargoID uuid.UUID) ([]domain.TrackingEntry, error) {
	if err := s.ensureCargoExists(ctx, cargoID); err != nil {
		return nil, err
	}

	entries, err := s.repos.Tracking.ListByCargo(ctx, cargoID)
	if err != nil {
		return nil, fmt.Errorf("list tracking for cargo %s: %w", cargoID, err)
	}
	return entries, nil
}

// Events returns the status-change events stored by the consumer, oldest first.
func (s *TrackingService) Events(ctx context.Context, cargoID uuid.UUID) ([]domain.CargoEvent, error) {
	if err := s.ensureCargoExists(ctx, cargoID); err != nil {
		return nil, err
	}

	events, err := s.repos.Events.ListByCargo(ctx, cargoID)
	if err != nil {
		return nil, fmt.Errorf("list events for cargo %s: %w", cargoID, err)
	}
	return events, nil
}

// ensureCargoExists turns "unknown cargo" into a 404 instead of an empty list.
func (s *TrackingService) ensureCargoExists(ctx context.Context, cargoID uuid.UUID) error {
	if _, err := s.repos.Cargoes.Get(ctx, cargoID); err != nil {
		return fmt.Errorf("get cargo %s: %w", cargoID, err)
	}
	return nil
}
