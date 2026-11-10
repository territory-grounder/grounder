CREATE TABLE control_window (
    seq           bigserial PRIMARY KEY,
    control_id    text NOT NULL UNIQUE CHECK (control_id <> ''),
    host          text NOT NULL CHECK (host <> ''),
    starts_at     timestamptz NOT NULL,
    ends_at       timestamptz NOT NULL,
    registered_at timestamptz NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at)
);
CREATE INDEX control_window_host_idx ON control_window (host, starts_at, ends_at);
COMMENT ON TABLE control_window IS 'plane: triage';
GRANT SELECT ON control_window TO tg_runtime;
