package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/drzbraz/deus.ai/internal/domain"
)

type CargoRepository struct {
	conn
}

func NewCargoRepository(pool *pgxpool.Pool) *CargoRepository {
	return &CargoRepository{conn{pool}}
}

const (
	cargoColumns = `id, name, description, weight_kg, status, vessel_id, version, created_at, updated_at`
	// cargoLocation is the location of the cargo's latest tracking entry,
	// served by the (cargo_id, recorded_at, id) index.
	cargoLocation = `COALESCE((SELECT t.location FROM tracking_entries t WHERE t.cargo_id = cargoes.id
	                           ORDER BY t.recorded_at DESC, t.id DESC LIMIT 1), '')`
)

func (r *CargoRepository) Create(ctx context.Context, c domain.Cargo) error {
	_, err := r.q(ctx).Exec(ctx,
		`INSERT INTO cargoes (`+cargoColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		c.ID, c.Name, c.Description, c.WeightKg, string(c.Status), c.VesselID, c.Version, c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return wrapErr("insert cargo", err)
	}
	return nil
}

func (r *CargoRepository) Get(ctx context.Context, id uuid.UUID) (domain.Cargo, error) {
	row := r.q(ctx).QueryRow(ctx, `SELECT `+cargoColumns+`, `+cargoLocation+` FROM cargoes WHERE id = $1`, id)

	cargo, err := scanCargo(row, true)
	if err != nil {
		return domain.Cargo{}, wrapErr("select cargo", err)
	}
	return cargo, nil
}

func (r *CargoRepository) GetForUpdate(ctx context.Context, id uuid.UUID) (domain.Cargo, error) {
	// No location here: under READ COMMITTED it would come from the snapshot
	// taken before waiting for the lock (see VesselRepository.GetForUpdate).
	row := r.q(ctx).QueryRow(ctx, `SELECT `+cargoColumns+` FROM cargoes WHERE id = $1 FOR UPDATE`, id)

	cargo, err := scanCargo(row, false)
	if err != nil {
		return domain.Cargo{}, wrapErr("lock cargo", err)
	}
	return cargo, nil
}

// List uses "($n IS NULL OR column = $n)" so one static, injection-safe query
// covers every combination of optional filters.
func (r *CargoRepository) List(ctx context.Context, f domain.CargoFilter) ([]domain.Cargo, error) {
	var status *string
	if f.Status != nil {
		s := string(*f.Status)
		status = &s
	}

	rows, err := r.q(ctx).Query(ctx,
		`SELECT `+cargoColumns+`, `+cargoLocation+` FROM cargoes
		 WHERE ($1::cargo_status IS NULL OR status = $1)
		   AND ($2::uuid IS NULL OR vessel_id = $2)
		 ORDER BY created_at, id
		 LIMIT $3 OFFSET $4`,
		status, f.VesselID, f.Page.Limit, f.Page.Offset)
	if err != nil {
		return nil, wrapErr("list cargoes", err)
	}
	return collectCargoes(rows, true, "list cargoes")
}

// ListForUpdateByVessel locks the vessel's cargoes in the given status, in id
// order. Under READ COMMITTED a row whose status changed while we waited for
// its lock is re-checked and skipped.
func (r *CargoRepository) ListForUpdateByVessel(ctx context.Context, vesselID uuid.UUID, status domain.Status) ([]domain.Cargo, error) {
	rows, err := r.q(ctx).Query(ctx,
		`SELECT `+cargoColumns+` FROM cargoes
		 WHERE vessel_id = $1 AND status = $2
		 ORDER BY id
		 FOR UPDATE`, vesselID, string(status))
	if err != nil {
		return nil, wrapErr("lock vessel cargoes", err)
	}
	return collectCargoes(rows, false, "lock vessel cargoes")
}

func collectCargoes(rows pgx.Rows, withLocation bool, op string) ([]domain.Cargo, error) {
	defer rows.Close()

	cargoes := []domain.Cargo{}
	for rows.Next() {
		cargo, err := scanCargo(rows, withLocation)
		if err != nil {
			return nil, wrapErr("scan cargo", err)
		}
		cargoes = append(cargoes, cargo)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapErr(op, err)
	}
	return cargoes, nil
}

func (r *CargoRepository) Update(ctx context.Context, c domain.Cargo) error {
	tag, err := r.q(ctx).Exec(ctx,
		`UPDATE cargoes SET status = $2, version = $3, updated_at = $4 WHERE id = $1`,
		c.ID, string(c.Status), c.Version, c.UpdatedAt)
	if err != nil {
		return wrapErr("update cargo", err)
	}
	if tag.RowsAffected() == 0 {
		return wrapErr("update cargo", pgx.ErrNoRows)
	}
	return nil
}

func scanCargo(row pgx.Row, withLocation bool) (domain.Cargo, error) {
	var (
		c      domain.Cargo
		status string
	)
	dest := []any{&c.ID, &c.Name, &c.Description, &c.WeightKg, &status, &c.VesselID, &c.Version, &c.CreatedAt, &c.UpdatedAt}
	if withLocation {
		dest = append(dest, &c.CurrentLocation)
	}
	err := row.Scan(dest...)
	c.Status = domain.Status(status)
	c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	return c, err //nolint:wrapcheck // callers wrap with wrapErr, which also classifies the error
}
