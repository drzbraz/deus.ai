package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/drzbraz/deus.ai/internal/domain"
)

// CargoEventRepository stores events received from the broker.
type CargoEventRepository struct {
	conn
}

func NewCargoEventRepository(pool *pgxpool.Pool) *CargoEventRepository {
	return &CargoEventRepository{conn{pool}}
}

// Save inserts the event once. A redelivered event hits the primary key and
// is ignored, so it reports inserted=false instead of failing.
func (r *CargoEventRepository) Save(ctx context.Context, e domain.StatusChanged) (inserted bool, err error) {
	tag, err := r.q(ctx).Exec(ctx,
		`INSERT INTO cargo_events (id, cargo_id, old_status, new_status, occurred_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (id) DO NOTHING`,
		e.EventID, e.CargoID, string(e.OldStatus), string(e.NewStatus), e.OccurredAt)
	if err != nil {
		return false, wrapErr("insert cargo event", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *CargoEventRepository) ListByCargo(ctx context.Context, cargoID uuid.UUID) ([]domain.CargoEvent, error) {
	rows, err := r.q(ctx).Query(ctx,
		`SELECT id, cargo_id, old_status, new_status, occurred_at, received_at
		 FROM cargo_events WHERE cargo_id = $1
		 ORDER BY occurred_at, id`, cargoID)
	if err != nil {
		return nil, wrapErr("list cargo events", err)
	}
	defer rows.Close()

	result := []domain.CargoEvent{}
	for rows.Next() {
		var (
			e                    domain.CargoEvent
			oldStatus, newStatus string
		)
		if err := rows.Scan(&e.EventID, &e.CargoID, &oldStatus, &newStatus, &e.OccurredAt, &e.ReceivedAt); err != nil {
			return nil, wrapErr("scan cargo event", err)
		}
		e.OldStatus, e.NewStatus = domain.Status(oldStatus), domain.Status(newStatus)
		e.OccurredAt, e.ReceivedAt = e.OccurredAt.UTC(), e.ReceivedAt.UTC()
		result = append(result, e)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapErr("list cargo events", err)
	}
	return result, nil
}
