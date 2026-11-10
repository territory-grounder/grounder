-- 0132: the INSTRUMENT identity a triage session ran under (TG-672, F23; spec/025 REQ-2535).
--
-- harness_fp (0115) names the prompt's system half and nothing else: the G3 soak cohort of 2026-11-06/07
-- carried ONE fingerprint over 140 sessions while eighteen behaviour-changing deploys landed inside it.
-- instrument_id is the SHA-256 over every configured component that can move a decision or its grade — build
-- SHA, harness fingerprint, op-schema catalog hash, policy bundle version, model alias map, rubric version,
-- production skill set, knowledge corpus head (core/instrumentid). The observed estate topology and the learner's
-- co-occurrence state are NOT hashed (they are the world being measured and move with every alert); provider
-- model versions behind an alias are not either (the judge records its own on session_judgment.judge_model). One
-- row per identity names its
-- components, and which were unrecorded ('' hashes as '' and is named in `unrecorded`), so a report can say
-- WHICH component differs between two identities without re-deriving anything.
--
-- plane: triage — the runner writes it beside session_triage on a session's first use of a configuration.
CREATE TABLE instrument_identity (
    instrument_id      text PRIMARY KEY CHECK (instrument_id ~ '^[0-9a-f]{64}$'),
    build_sha          text NOT NULL DEFAULT '',
    harness_fp         text NOT NULL DEFAULT '',
    opschema_hash      text NOT NULL DEFAULT '',
    policy_bundle      text NOT NULL DEFAULT '',
    model_alias_hash   text NOT NULL DEFAULT '',
    rubric_version     text NOT NULL DEFAULT '',
    skill_set_hash     text NOT NULL DEFAULT '',
    knowledge_hash     text NOT NULL DEFAULT '',
    unrecorded         text NOT NULL DEFAULT '',
    first_seen         timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE instrument_identity IS 'plane: triage';
COMMENT ON COLUMN instrument_identity.unrecorded IS
  'TG-672: comma-joined names of the components the process could not read when this identity was first seen (empty = complete)';

-- The per-session and per-run stamps. Additive, defaulted, NON-SECRET; pre-0132 rows read '' (unrecorded).
ALTER TABLE session_triage ADD COLUMN instrument_id text NOT NULL DEFAULT '';
ALTER TABLE triage_run    ADD COLUMN instrument_id text NOT NULL DEFAULT '';
CREATE INDEX session_triage_instrument ON session_triage (instrument_id) WHERE instrument_id <> '';
COMMENT ON COLUMN session_triage.instrument_id IS
  'TG-672: the instrument identity (core/instrumentid) the session ran under; empty = recorded before 0132';
COMMENT ON COLUMN triage_run.instrument_id IS
  'TG-672: the instrument identity (core/instrumentid) the run ran under; empty = recorded before 0132';
