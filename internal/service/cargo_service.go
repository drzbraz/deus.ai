package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/drzbraz/deus.ai/internal/domain"
)

// CargoService manages cargo creation, lookup and the status lifecycle.
type CargoService struct {
	repos   Repositories
	now     Clock
	metrics TransitionRecorder
}

func NewCargoService(repos Repositories, clock Clock, metrics TransitionRecorder) *CargoService {
	return &CargoService{repos: repos, now: clock, metrics: metrics}
}

type CreateCargoInput struct {
	Name        string
	Description string
	WeightKg    float64
	VesselID    uuid.UUID
}

// Create registers a pending cargo on a vessel and records the first
// tracking entry at the vessel's current location.
func (s *CargoService) Create(ctx context.Context, in CreateCargoInput) (domain.Cargo, error) {
	now := s.now()
	cargo, err := domain.NewCargo(domain.NewCargoParams(in), now)
	if err != nil {
		return domain.Cargo{}, err
	}

	err = s.repos.Tx.WithinTx(ctx, func(ctx context.Context) error {
		vessel, err := s.loadVesselForCargo(ctx, cargo.VesselID)
		if err != nil {
			return err
		}
		if !vessel.CanLoad(vessel.LoadKg, cargo.WeightKg) {
			return fmt.Errorf("%w: vessel %s has %.2f kg free, cargo weighs %.2f kg",
				domain.ErrCapacityExceeded, vessel.ID, vessel.CapacityKg-vessel.LoadKg, cargo.WeightKg)
		}

		entry, err := domain.NewTrackingEntry(cargo, vessel.CurrentLocation, "cargo registered", now)
		if err != nil {
			return err
		}
		if err := s.repos.Cargoes.Create(ctx, cargo); err != nil {
			return err
		}
		return s.repos.Tracking.Append(ctx, entry)
	})
	if err != nil {
		return domain.Cargo{}, fmt.Errorf("create cargo: %w", err)
	}
	return cargo, nil
}

// loadVesselForCargo locks the vessel so concurrent cargo creation cannot
// overbook its capacity. An unknown vessel is a client input problem (422),
// not a missing resource (404), because the URL itself was valid.
func (s *CargoService) loadVesselForCargo(ctx context.Context, vesselID uuid.UUID) (domain.Vessel, error) {
	vessel, err := s.repos.Vessels.GetForUpdate(ctx, vesselID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Vessel{}, domain.NewValidationError("vessel_id", "vessel does not exist")
	}
	return vessel, err
}

func (s *CargoService) Get(ctx context.Context, id uuid.UUID) (domain.Cargo, error) {
	cargo, err := s.repos.Cargoes.Get(ctx, id)
	if err != nil {
		return domain.Cargo{}, fmt.Errorf("get cargo %s: %w", id, err)
	}
	return cargo, nil
}

func (s *CargoService) List(ctx context.Context, filter domain.CargoFilter) ([]domain.Cargo, error) {
	cargoes, err := s.repos.Cargoes.List(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("list cargoes: %w", err)
	}
	return cargoes, nil
}

type UpdateStatusInput struct {
	CargoID uuid.UUID
	Status  string
	// Location of the movement; defaults to the vessel's current location.
	Location string
	Note     string
	// ExpectedVersion enables optimistic concurrency (If-Match). Zero skips the check.
	ExpectedVersion int
}

// UpdateStatus moves a cargo through its lifecycle. The new status, the
// tracking entry and the outbox event are committed atomically, so an event
// is published if and only if the change really happened.
func (s *CargoService) UpdateStatus(ctx context.Context, in UpdateStatusInput) (domain.Cargo, error) {
	next, err := domain.ParseStatus(in.Status)
	if err != nil {
		return domain.Cargo{}, err
	}

	var (
		updated domain.Cargo
		event   domain.StatusChanged
	)
	err = s.repos.Tx.WithinTx(ctx, func(ctx context.Context) error {
		cargo, err := s.repos.Cargoes.GetForUpdate(ctx, in.CargoID)
		if err != nil {
			return err
		}
		if in.ExpectedVersion != 0 && cargo.Version != in.ExpectedVersion {
			return fmt.Errorf("%w: expected version %d, current is %d", domain.ErrVersionMismatch, in.ExpectedVersion, cargo.Version)
		}

		now := s.now()
		event, err = cargo.ChangeStatus(next, now)
		if err != nil {
			return err
		}

		entry, err := s.newMovement(ctx, cargo, in.Location, in.Note)
		if err != nil {
			return err
		}
		if err := s.repos.Cargoes.Update(ctx, cargo); err != nil {
			return err
		}
		if err := s.repos.Tracking.Append(ctx, entry); err != nil {
			return err
		}
		if err := s.repos.Outbox.Enqueue(ctx, event); err != nil {
			return err
		}

		updated = cargo
		return nil
	})
	if err != nil {
		return domain.Cargo{}, fmt.Errorf("update cargo %s status: %w", in.CargoID, err)
	}

	s.metrics.RecordTransition(ctx, event.OldStatus, event.NewStatus)
	return updated, nil
}

// newMovement builds the tracking entry for a status change, falling back to
// the vessel's current location when the client did not provide one.
func (s *CargoService) newMovement(ctx context.Context, cargo domain.Cargo, location, note string) (domain.TrackingEntry, error) {
	if location == "" {
		vessel, err := s.repos.Vessels.Get(ctx, cargo.VesselID)
		if err != nil {
			return domain.TrackingEntry{}, err
		}
		location = vessel.CurrentLocation
	}
	return domain.NewTrackingEntry(cargo, location, note, cargo.UpdatedAt)
}
