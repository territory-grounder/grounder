CREATE TABLE actuation_dispatch (
    dispatch_id  text PRIMARY KEY CHECK (dispatch_id <> ''),
    action_id    text NOT NULL,
    external_ref text NOT NULL,
    claimed_at   timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE actuation_dispatch IS 'plane: actuation';
GRANT SELECT, INSERT ON actuation_dispatch TO tg_runtime;
