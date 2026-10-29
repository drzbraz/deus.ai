package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/drzbraz/deus.ai/internal/domain"
)

// VesselService manages vessels.
type VesselService struct {
	repos Repositories
	now   Clock
}

func NewVesselService(repos Repositories, clock Clock) *VesselService {
	return &VesselService{repos: repos, now: clock}
}

type CreateVesselInput struct {
	Name            string
	CapacityKg      float64
	CurrentLocation string
}

func (s *VesselService) Create(ctx context.Context, in CreateVesselInput) (domain.Vessel, error) {
	vessel, err := domain.NewVessel(in.Name, in.CapacityKg, in.CurrentLocation, s.now())
	if err != nil {
		return domain.Vessel{}, err
	}

	if err := s.repos.Vessels.Create(ctx, vessel); err != nil {
		return domain.Vessel{}, fmt.Errorf("create vessel: %w", err)
	}
	return vessel, nil
}

func (s *VesselService) Get(ctx context.Context, id uuid.UUID) (domain.Vessel, error) {
	vessel, err := s.repos.Vessels.Get(ctx, id)
	if err != nil {
		return domain.Vessel{}, fmt.Errorf("get vessel %s: %w", id, err)
	}
	return vessel, nil
}

func (s *VesselService) List(ctx context.Context, page domain.Page) ([]domain.Vessel, error) {
	vessels, err := s.repos.Vessels.List(ctx, page)
	if err != nil {
		return nil, fmt.Errorf("list vessels: %w", err)
	}
	return vessels, nil
}

func (s *VesselService) UpdateLocation(ctx context.Context, id uuid.UUID, location string) (domain.Vessel, error) {
	var updated domain.Vessel

	err := s.repos.Tx.WithinTx(ctx, func(ctx context.Context) error {
		vessel, err := s.repos.Vessels.GetForUpdate(ctx, id)
		if err != nil {
			return err
		}
		if err := vessel.MoveTo(location, s.now()); err != nil {
			return err
		}
		if err := s.repos.Vessels.Update(ctx, vessel); err != nil {
			return err
		}
		updated = vessel
		return nil
	})
	if err != nil {
		return domain.Vessel{}, fmt.Errorf("update vessel %s location: %w", id, err)
	}
	return updated, nil
}
