-- Stores the first response for each Idempotency-Key so client retries and
-- concurrent duplicates get the same answer without repeating side effects.
-- The body is stored as raw bytes so a replay is byte-for-byte identical.
CREATE TABLE idempotency_keys (
    scope            TEXT        NOT NULL,
    key              TEXT        NOT NULL,
    request_hash     TEXT        NOT NULL,
    status_code      INTEGER     NOT NULL,
    response_headers JSONB       NOT NULL DEFAULT '{}'::jsonb,
    response_body    BYTEA       NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, key)
);

CREATE INDEX idempotency_keys_created_at_idx ON idempotency_keys (created_at);
