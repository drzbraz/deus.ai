CREATE TABLE vessels (
    id               UUID PRIMARY KEY,
    name             TEXT          NOT NULL,
    capacity_kg      NUMERIC(12,2) NOT NULL CHECK (capacity_kg > 0),
    current_location TEXT          NOT NULL CHECK (current_location <> ''),
    created_at       TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT vessels_name_key UNIQUE (name),
    CONSTRAINT vessels_name_not_blank CHECK (name <> '')
);
