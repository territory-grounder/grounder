-- TG-710: the fault injector's memory guard refuses a target on a guarded node when that node lacks room to take
-- the target's memory back on restore. Each refusal is recorded here, apart from injected_fault: a skipped
-- target was never faulted, so it is in no fault denominator, and injected_fault keeps meaning "what was broken".
CREATE TABLE IF NOT EXISTS injection_skip (
    seq               bigserial PRIMARY KEY,
    skipped_at        timestamptz NOT NULL DEFAULT now(),
    host              text NOT NULL CHECK (host <> ''),
    fault_type        text NOT NULL CHECK (fault_type <> ''),
    node              text NOT NULL CHECK (node <> ''),
    stage             text NOT NULL CHECK (stage IN ('plan', 'pre-effect')),
    reason_key        text NOT NULL CHECK (reason_key IN ('telemetry-unavailable', 'telemetry-stale', 'memory-pressure',
                                                          'below-threshold', 'no-declared-cost',
                                                          'outstanding-without-cost', 'obligations-unreadable')),
    detail            text NOT NULL DEFAULT '',
    available_bytes   bigint CHECK (available_bytes IS NULL OR available_bytes >= 0),
    required_bytes    bigint CHECK (required_bytes IS NULL OR required_bytes > 0),
    psi_some_avg60    double precision CHECK (psi_some_avg60 IS NULL OR psi_some_avg60 >= 0),
    telemetry_read_at timestamptz,
    note              text NOT NULL DEFAULT '',
    schema_version    int NOT NULL DEFAULT 1 CHECK (schema_version > 0)
);
CREATE INDEX IF NOT EXISTS injection_skip_at ON injection_skip (skipped_at);

-- plane: both — benchmark instrumentation of the injected_fault class (0038/0060): it records why the harness
-- passed over a target, authorises nothing, and is read by the campaign report.
COMMENT ON TABLE injection_skip IS 'plane: both';
GRANT SELECT, INSERT ON injection_skip TO tg_runtime;
