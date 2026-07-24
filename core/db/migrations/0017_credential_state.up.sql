-- 0017 credential state — the WORKER's live credential-engine coverage + sync state, PUBLISHED for the
-- console (spec/016 credential engine, wired live in the worker). The worker instantiates the SyncEngine
-- from config, runs each source's read-only Sync on a schedule, and writes here, per sync: one append-only
-- credential_sync_run row (the SyncRun shape — source, plane, last-synced, drift counts, outcome, non-secret
-- error text) and one latest-wins credential_coverage row per source (how many targets it currently covers).
--
-- NEVER A SECRET: the SyncRun / coverage projection types are secret-free BY CONSTRUCTION (only counts and
-- non-secret metadata cross over — a credential source stores REFERENCES, never values, INV-13). No column
-- here can hold secret material.
--
-- Like runtime_posture (0016), this is OPERATIONAL projection state for the console, NOT a governed audit
-- row: the governance_ledger remains the authoritative decision record, and nothing here can enable a
-- capability. Hence no schema_version stamp (cf. runtime_posture / operator_sessions): a stale/absent row the
-- reader treats as UNKNOWN, never a confident false. credential_sync_run is append-only (the worker only
-- INSERTs, latest-per-source is a read concern); credential_coverage is one row per source (single writer).

CREATE TABLE credential_sync_run (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  source_id      text NOT NULL CHECK (length(btrim(source_id)) > 0),
  plane          text NOT NULL CHECK (plane IN ('machine', 'human')),
  started_at     timestamptz NOT NULL,
  last_synced_at timestamptz,                                        -- the last SUCCESSFUL sync; NULL if never
  added          int NOT NULL DEFAULT 0 CHECK (added >= 0),
  changed        int NOT NULL DEFAULT 0 CHECK (changed >= 0),
  removed        int NOT NULL DEFAULT 0 CHECK (removed >= 0),
  outcome        text NOT NULL CHECK (outcome IN ('ok', 'failed')),
  err            text NOT NULL DEFAULT '',                           -- non-secret error text; empty on ok
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX credential_sync_run_source_created_idx ON credential_sync_run (source_id, created_at DESC);

CREATE TABLE credential_coverage (
  source_id  text PRIMARY KEY CHECK (length(btrim(source_id)) > 0),
  plane      text NOT NULL CHECK (plane IN ('machine', 'human')),
  targets    int NOT NULL DEFAULT 0 CHECK (targets >= 0),           -- entries this source currently covers
  updated_at timestamptz NOT NULL DEFAULT now()
);
