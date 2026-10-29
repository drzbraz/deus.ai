package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/drzbraz/deus.ai/internal/domain"
)

type VesselRepository struct {
	conn
}

func NewVesselRepository(pool *pgxpool.Pool) *VesselRepository {
	return &VesselRepository{conn{pool}}
}

const (
	vesselColumns = `v.id, v.name, v.capacity_kg, v.current_location, v.created_at, v.updated_at`
	// vesselLoad sums the weight of every cargo still on board.
	vesselLoad = `COALESCE((SELECT SUM(c.weight_kg) FROM cargoes c
	                        WHERE c.vessel_id = v.id AND c.status <> 'delivered'), 0)`
)

func (r *VesselRepository) Create(ctx context.Context, v domain.Vessel) error {
	_, err := r.q(ctx).Exec(ctx,
		`INSERT INTO vessels (id, name, capacity_kg, current_location, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		v.ID, v.Name, v.CapacityKg, v.CurrentLocation, v.CreatedAt, v.UpdatedAt)
	if err != nil {
		return wrapErr("insert vessel", err)
	}
	return nil
}

func (r *VesselRepository) Get(ctx context.Context, id uuid.UUID) (domain.Vessel, error) {
	row := r.q(ctx).QueryRow(ctx,
		`SELECT `+vesselColumns+`, `+vesselLoad+` FROM vessels v WHERE v.id = $1`, id)

	vessel, err := scanVessel(row, true)
	if err != nil {
		return domain.Vessel{}, wrapErr("select vessel", err)
	}
	return vessel, nil
}

// GetForUpdate locks the vessel and then computes its load in a second
// statement. Under READ COMMITTED a single "SELECT ... FOR UPDATE" would
// evaluate the load subquery with the snapshot taken *before* waiting for the
// lock, missing cargo committed by the transaction we waited on, and the
// vessel could be overbooked.
func (r *VesselRepository) GetForUpdate(ctx context.Context, id uuid.UUID) (domain.Vessel, error) {
	row := r.q(ctx).QueryRow(ctx,
		`SELECT `+vesselColumns+` FROM vessels v WHERE v.id = $1 FOR UPDATE`, id)

	vessel, err := scanVessel(row, false)
	if err != nil {
		return domain.Vessel{}, wrapErr("lock vessel", err)
	}

	err = r.q(ctx).QueryRow(ctx,
		`SELECT `+vesselLoad+` FROM vessels v WHERE v.id = $1`, id).Scan(&vessel.LoadKg)
	if err != nil {
		return domain.Vessel{}, wrapErr("compute vessel load", err)
	}
	return vessel, nil
}

func (r *VesselRepository) List(ctx context.Context, page domain.Page) ([]domain.Vessel, error) {
	rows, err := r.q(ctx).Query(ctx,
		`SELECT `+vesselColumns+`, `+vesselLoad+` FROM vessels v
		 ORDER BY v.created_at, v.id LIMIT $1 OFFSET $2`,
		page.Limit, page.Offset)
	if err != nil {
		return nil, wrapErr("list vessels", err)
	}
	defer rows.Close()

	vessels := []domain.Vessel{}
	for rows.Next() {
		vessel, err := scanVessel(rows, true)
		if err != nil {
			return nil, wrapErr("scan vessel", err)
		}
		vessels = append(vessels, vessel)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapErr("list vessels", err)
	}
	return vessels, nil
}

func (r *VesselRepository) Update(ctx context.Context, v domain.Vessel) error {
	tag, err := r.q(ctx).Exec(ctx,
		`UPDATE vessels SET current_location = $2, updated_at = $3 WHERE id = $1`,
		v.ID, v.CurrentLocation, v.UpdatedAt)
	if err != nil {
		return wrapErr("update vessel", err)
	}
	if tag.RowsAffected() == 0 {
		return wrapErr("update vessel", pgx.ErrNoRows)
	}
	return nil
}

func scanVessel(row pgx.Row, withLoad bool) (domain.Vessel, error) {
	var v domain.Vessel
	dest := []any{&v.ID, &v.Name, &v.CapacityKg, &v.CurrentLocation, &v.CreatedAt, &v.UpdatedAt}
	if withLoad {
		dest = append(dest, &v.LoadKg)
	}
	err := row.Scan(dest...)
	v.CreatedAt, v.UpdatedAt = v.CreatedAt.UTC(), v.UpdatedAt.UTC()
	return v, err //nolint:wrapcheck // callers wrap with wrapErr, which also classifies the error
}
