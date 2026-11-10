CREATE TABLE IF NOT EXISTS actuation_state_read (
    seq            bigserial PRIMARY KEY,
    host           text NOT NULL CHECK (host <> ''),
    purpose        text NOT NULL CHECK (purpose <> ''),
    read_at        timestamptz NOT NULL DEFAULT now(),
    schema_version int NOT NULL DEFAULT 1 CHECK (schema_version > 0)
);
CREATE INDEX IF NOT EXISTS actuation_state_read_host_at ON actuation_state_read (lower(host), read_at);
COMMENT ON TABLE actuation_state_read IS 'plane: actuation';
GRANT SELECT, INSERT ON actuation_state_read TO tg_runtime;
