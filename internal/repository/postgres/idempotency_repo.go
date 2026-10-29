package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/drzbraz/deus.ai/internal/idempotency"
)

// IdempotencyRepository implements idempotency.Store. It must be called with
// a ctx that carries a transaction (see TxManager).
type IdempotencyRepository struct {
	conn
}

func NewIdempotencyRepository(pool *pgxpool.Pool) *IdempotencyRepository {
	return &IdempotencyRepository{conn{pool}}
}

// pendingStatusCode marks a claimed key whose response is not stored yet.
// It is never visible to other transactions because the claim and the
// completion are committed together.
const pendingStatusCode = 0

func (r *IdempotencyRepository) Claim(ctx context.Context, scope, key, requestHash string) (idempotency.Record, bool, error) {
	// The INSERT blocks on the primary key while another transaction holds the
	// same key, which is exactly what serialises concurrent duplicates.
	tag, err := r.q(ctx).Exec(ctx,
		`INSERT INTO idempotency_keys (scope, key, request_hash, status_code, response_body)
		 VALUES ($1, $2, $3, $4, ''::bytea)
		 ON CONFLICT (scope, key) DO NOTHING`,
		scope, key, requestHash, pendingStatusCode)
	if err != nil {
		return idempotency.Record{}, false, wrapErr("claim idempotency key", err)
	}
	if tag.RowsAffected() == 1 {
		return idempotency.Record{}, true, nil
	}

	existing := idempotency.Record{Scope: scope, Key: key}
	err = r.q(ctx).QueryRow(ctx,
		`SELECT request_hash, status_code, response_headers, response_body FROM idempotency_keys
		 WHERE scope = $1 AND key = $2`, scope, key).
		Scan(&existing.RequestHash, &existing.StatusCode, &existing.Headers, &existing.Body)
	if err != nil {
		return idempotency.Record{}, false, wrapErr("load idempotency key", err)
	}
	return existing, false, nil
}

func (r *IdempotencyRepository) Complete(ctx context.Context, rec idempotency.Record) error {
	headers := rec.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	tag, err := r.q(ctx).Exec(ctx,
		`UPDATE idempotency_keys SET status_code = $3, response_headers = $4, response_body = $5
		 WHERE scope = $1 AND key = $2`,
		rec.Scope, rec.Key, rec.StatusCode, headers, rec.Body)
	if err != nil {
		return wrapErr("store idempotent response", err)
	}
	if tag.RowsAffected() == 0 {
		return wrapErr("store idempotent response", pgx.ErrNoRows)
	}
	return nil
}

func (r *IdempotencyRepository) DeleteExpired(ctx context.Context, createdBefore time.Time) (int64, error) {
	tag, err := r.q(ctx).Exec(ctx, `DELETE FROM idempotency_keys WHERE created_at < $1`, createdBefore)
	if err != nil {
		return 0, wrapErr("delete expired idempotency keys", err)
	}
	return tag.RowsAffected(), nil
}
