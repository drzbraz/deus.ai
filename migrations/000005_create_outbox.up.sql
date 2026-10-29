-- Transactional outbox: events are written in the same transaction as the
-- state change and published to the broker asynchronously by the relay.
CREATE TABLE outbox (
    id           UUID PRIMARY KEY,
    aggregate_id UUID        NOT NULL,
    event_type   TEXT        NOT NULL,
    payload      JSONB       NOT NULL,
    -- W3C trace context so the trace continues from the HTTP request to the consumer.
    headers      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ,
    attempts     INTEGER     NOT NULL DEFAULT 0,
    last_error   TEXT
);

-- The relay only ever scans unpublished rows, in creation order.
CREATE INDEX outbox_unpublished_idx ON outbox (created_at, id) WHERE published_at IS NULL;
