-- Filled by the event consumer. The primary key is the producer's event id,
-- which makes consumption idempotent under at-least-once delivery.
CREATE TABLE cargo_events (
    id          UUID PRIMARY KEY,
    cargo_id    UUID         NOT NULL REFERENCES cargoes (id) ON DELETE RESTRICT,
    old_status  cargo_status NOT NULL,
    new_status  cargo_status NOT NULL,
    occurred_at TIMESTAMPTZ  NOT NULL,
    received_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX cargo_events_cargo_id_occurred_at_idx ON cargo_events (cargo_id, occurred_at, id);

CREATE TRIGGER cargo_events_append_only
    BEFORE UPDATE OR DELETE ON cargo_events
    FOR EACH ROW EXECUTE FUNCTION forbid_audit_modification();

CREATE TRIGGER cargo_events_no_truncate
    BEFORE TRUNCATE ON cargo_events
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_audit_modification();
