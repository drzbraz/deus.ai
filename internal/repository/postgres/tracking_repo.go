package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/drzbraz/deus.ai/internal/domain"
)

// TrackingRepository only appends: the table is protected by an
// append-only trigger, so history can never be rewritten.
type TrackingRepository struct {
	conn
}

func NewTrackingRepository(pool *pgxpool.Pool) *TrackingRepository {
	return &TrackingRepository{conn{pool}}
}

func (r *TrackingRepository) Append(ctx context.Context, e domain.TrackingEntry) error {
	_, err := r.q(ctx).Exec(ctx,
		`INSERT INTO tracking_entries (id, cargo_id, status, location, note, recorded_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		e.ID, e.CargoID, string(e.Status), e.Location, e.Note, e.RecordedAt)
	if err != nil {
		return wrapErr("insert tracking entry", err)
	}
	return nil
}

func (r *TrackingRepository) ListByCargo(ctx context.Context, cargoID uuid.UUID) ([]domain.TrackingEntry, error) {
	rows, err := r.q(ctx).Query(ctx,
		`SELECT id, cargo_id, status, location, note, recorded_at
		 FROM tracking_entries WHERE cargo_id = $1
		 ORDER BY recorded_at, id`, cargoID)
	if err != nil {
		return nil, wrapErr("list tracking entries", err)
	}
	defer rows.Close()

	entries := []domain.TrackingEntry{}
	for rows.Next() {
		var (
			e      domain.TrackingEntry
			status string
		)
		if err := rows.Scan(&e.ID, &e.CargoID, &status, &e.Location, &e.Note, &e.RecordedAt); err != nil {
			return nil, wrapErr("scan tracking entry", err)
		}
		e.Status = domain.Status(status)
		e.RecordedAt = e.RecordedAt.UTC()
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapErr("list tracking entries", err)
	}
	return entries, nil
}
