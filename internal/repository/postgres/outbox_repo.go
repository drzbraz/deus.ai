package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/drzbraz/deus.ai/internal/domain"
	"github.com/drzbraz/deus.ai/internal/events"
)

// maxErrorLength keeps a flapping broker from bloating the outbox table.
const maxErrorLength = 500

type OutboxRepository struct {
	conn
}

func NewOutboxRepository(pool *pgxpool.Pool) *OutboxRepository {
	return &OutboxRepository{conn{pool}}
}

// Enqueue stores the event in the caller's transaction. The outbox row id is
// the event id, so a message can be traced end-to-end by a single identifier.
func (r *OutboxRepository) Enqueue(ctx context.Context, event domain.StatusChanged) error {
	payload, err := events.EncodeStatusChanged(event)
	if err != nil {
		return err
	}

	traceHeaders := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, traceHeaders)
	headers, err := json.Marshal(traceHeaders)
	if err != nil {
		return wrapErr("encode outbox headers", err)
	}

	_, err = r.q(ctx).Exec(ctx,
		`INSERT INTO outbox (id, aggregate_id, event_type, payload, headers, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		event.EventID, event.CargoID, events.RoutingKeyStatusChanged, payload, headers, event.OccurredAt)
	if err != nil {
		return wrapErr("insert outbox message", err)
	}
	return nil
}

// FetchUnpublished claims the oldest pending messages. SKIP LOCKED lets
// several relay instances share the outbox without publishing a row twice.
func (r *OutboxRepository) FetchUnpublished(ctx context.Context, limit int) ([]events.OutboxMessage, error) {
	rows, err := r.q(ctx).Query(ctx,
		`SELECT id, event_type, payload, headers, attempts FROM outbox
		 WHERE published_at IS NULL
		 ORDER BY created_at, id
		 LIMIT $1
		 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, wrapErr("fetch outbox messages", err)
	}
	defer rows.Close()

	var messages []events.OutboxMessage
	for rows.Next() {
		var msg events.OutboxMessage
		if err := rows.Scan(&msg.ID, &msg.RoutingKey, &msg.Payload, &msg.Headers, &msg.Attempts); err != nil {
			return nil, wrapErr("scan outbox message", err)
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapErr("fetch outbox messages", err)
	}
	return messages, nil
}

func (r *OutboxRepository) MarkPublished(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := r.q(ctx).Exec(ctx, `UPDATE outbox SET published_at = $2 WHERE id = $1`, id, at)
	if err != nil {
		return wrapErr("mark outbox message published", err)
	}
	return nil
}

func (r *OutboxRepository) MarkFailed(ctx context.Context, id uuid.UUID, reason string) error {
	if len(reason) > maxErrorLength {
		reason = reason[:maxErrorLength]
	}
	_, err := r.q(ctx).Exec(ctx,
		`UPDATE outbox SET attempts = attempts + 1, last_error = $2 WHERE id = $1`, id, reason)
	if err != nil {
		return wrapErr("mark outbox message failed", err)
	}
	return nil
}

// CountPending feeds the outbox backlog gauge: a growing number means the
// broker is unreachable or the relay is falling behind.
func (r *OutboxRepository) CountPending(ctx context.Context) (int64, error) {
	var count int64
	err := r.q(ctx).QueryRow(ctx, `SELECT COUNT(*) FROM outbox WHERE published_at IS NULL`).Scan(&count)
	if err != nil {
		return 0, wrapErr("count pending outbox messages", err)
	}
	return count, nil
}
