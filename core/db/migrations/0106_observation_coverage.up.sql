-- 0106: the observation-coverage SNAPSHOT (TG-180 — "coverage of the unmeasured" as a scorecard dimension).
--
-- The census (TG-180 part 1) and the probe ledger (0093, part 2) live on the WORKER: the census denominator is
-- computed from the in-memory estate graph at scrape time, so it reaches Prometheus and nothing else. The
-- grounding scorecard (core/axis) is produced by the GROUNDER from durable rows, which is why the ticket's
-- own acceptance — "coverage-of-the-unmeasured published as a grounding-scorecard dimension" — was unmet
-- while every gauge already existed. This table is the bridge: the worker appends one snapshot per census
-- refresh (total / observed / healthy-quiet / unobservable / probe-confirmed / armed), the grounder reads the
-- latest. APPEND-ONLY: a snapshot is a measurement at an instant, never edited.
CREATE TABLE observation_coverage (
  id              bigserial PRIMARY KEY,
  recorded_at     timestamptz NOT NULL DEFAULT now(),
  total           int NOT NULL CHECK (total >= 0),
  observed        int NOT NULL CHECK (observed >= 0),
  healthy_quiet   int NOT NULL CHECK (healthy_quiet >= 0),
  unobservable    int NOT NULL CHECK (unobservable >= 0),
  confirmed       int NOT NULL CHECK (confirmed >= 0),
  probe_armed     boolean NOT NULL DEFAULT false,
  schema_version  int NOT NULL DEFAULT 1 CHECK (schema_version > 0)
);
CREATE INDEX observation_coverage_recorded_idx ON observation_coverage (recorded_at DESC);

-- plane: both — OBSERVATION instrumentation like observation_probe (0093): a measurement of the estate's
-- observability, written by the worker's census job, read by the grounder's axis scorer.
COMMENT ON TABLE observation_coverage IS 'plane: both';

-- Append-only by grant, explicitly (0015's convention), AND the default-privilege fix that 0105's inversion
-- needs on an ALREADY-INITIALISED database: postgres-init's ALTER DEFAULT PRIVILEGES runs only at first init,
-- so on a live estate every table created after 0105 would still be born with the old wildcard grant. This
-- rewrites the default here, inside the versioned lattice, so future tables are born SELECT+INSERT only.
-- Guarded: CI/test fixtures migrate as the superuser with tg_runtime created as a bare role for the GRANTs
-- to parse, but without a tg_migration role — the default-privilege rewrite applies only where the owning
-- role exists (every real deployment), and is a no-op elsewhere rather than a failed migration.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tg_migration') THEN
    EXECUTE 'ALTER DEFAULT PRIVILEGES FOR ROLE tg_migration IN SCHEMA public REVOKE UPDATE, DELETE ON TABLES FROM tg_runtime';
  END IF;
END $$;
REVOKE UPDATE, DELETE ON observation_coverage FROM tg_runtime;
GRANT SELECT, INSERT ON observation_coverage TO tg_runtime;
GRANT USAGE, SELECT ON SEQUENCE observation_coverage_id_seq TO tg_runtime;
