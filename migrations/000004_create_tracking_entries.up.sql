CREATE TABLE tracking_entries (
    id          UUID PRIMARY KEY,
    cargo_id    UUID         NOT NULL REFERENCES cargoes (id) ON DELETE RESTRICT,
    status      cargo_status NOT NULL,
    location    TEXT         NOT NULL CHECK (location <> ''),
    note        TEXT         NOT NULL DEFAULT '',
    recorded_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX tracking_entries_cargo_id_recorded_at_idx ON tracking_entries (cargo_id, recorded_at, id);

CREATE TRIGGER tracking_entries_append_only
    BEFORE UPDATE OR DELETE ON tracking_entries
    FOR EACH ROW EXECUTE FUNCTION forbid_audit_modification();

CREATE TRIGGER tracking_entries_no_truncate
    BEFORE TRUNCATE ON tracking_entries
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_audit_modification();
