-- WORKER-PUBLISHED CAPABILITY PROJECTION (TG-251).
--
-- The modules page answers "is this connector on?" from the API process's own registry — which holds 15
-- entries, while every notifier, tracker, cmdb, credsource, discovery and knowledge connector lives in the
-- WORKER. Absence was rendered as enabled:false: production showed notifier/matrix switched off while it
-- was delivering governance polls. MR !866 made unknown read as unknown (enabled_known); THIS table is the
-- real channel: the worker publishes its Capabilities() projection, the API reads it.
--
-- observed_at is the load-bearing column. The reader treats a row older than its staleness window as
-- ABSENT — a projection that outlives its publisher would serve a remembered answer, which is the same
-- defect one layer down.
CREATE TABLE module_capability_projection (
  surface        text        NOT NULL CHECK (length(btrim(surface)) > 0),
  source_type    text        NOT NULL CHECK (length(btrim(source_type)) > 0),
  capability     text        NOT NULL DEFAULT '',
  enabled        boolean     NOT NULL,
  observed_at    timestamptz NOT NULL DEFAULT now(),
  schema_version int         NOT NULL DEFAULT 1 CHECK (schema_version > 0),
  PRIMARY KEY (surface, source_type)
);

COMMENT ON TABLE module_capability_projection IS
  'Worker-published module enablement (TG-251): one row per (surface, source_type), refreshed on a heartbeat. Readers MUST apply a staleness cutoff on observed_at — a stale row is an unknown, never a remembered answer.';

GRANT SELECT, INSERT, UPDATE ON module_capability_projection TO tg_runtime;
