CREATE TYPE cargo_status AS ENUM ('pending', 'in_transit', 'delivered');

CREATE TABLE cargoes (
    id          UUID PRIMARY KEY,
    name        TEXT          NOT NULL CHECK (name <> ''),
    description TEXT          NOT NULL DEFAULT '',
    weight_kg   NUMERIC(12,2) NOT NULL CHECK (weight_kg > 0),
    status      cargo_status  NOT NULL DEFAULT 'pending',
    vessel_id   UUID          NOT NULL REFERENCES vessels (id) ON DELETE RESTRICT,
    version     INTEGER       NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at  TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ   NOT NULL DEFAULT now()
);

-- Supports the vessel load calculation and filtering cargo by vessel/status.
CREATE INDEX cargoes_vessel_id_status_idx ON cargoes (vessel_id, status);
CREATE INDEX cargoes_status_idx ON cargoes (status);
CREATE INDEX cargoes_created_at_idx ON cargoes (created_at, id);
