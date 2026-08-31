-- 0050: widen the graduation ladder for the earned-catalog rung (spec/028 REQ-2804, epic TG-227).
--
-- THE NEW RUNG. The ladder was two-valued: approve ("asks first") and auto ("acts silently"). Between them
-- there is a rung the predecessor had and TG did not — ACTS AND PAGES: the class executes, and a human is
-- notified in parallel and can veto. That is the rung an earned capability should sit at for a long time, and
-- it is the CEILING for any class admitted through the runtime overlay (ADR-0016 decision 2): the silent rung
-- stays reserved for classes present in the embedded, lockstep-hashed opschema.json — a code release.
--
-- FORWARD-SAFE BY THE 0040 PRECEDENT. Drop the inline CHECK, re-add it widened. An OLD worker reading a row
-- that says 'auto_notice' parses it as an unknown level, and core/db/policy_graduation_store.go resolves an
-- unknown level to APPROVE (fail closed) — a mid-rollout worker downgrades autonomy rather than inventing it.
-- That is why the level column is text and not an enum type: widening must never require every reader to be
-- upgraded first.
--
-- notice_run_count IS A SECOND, SEPARATE STREAK. Promotion approve -> auto_notice spends clean_run_count;
-- the climb auto_notice -> auto is counted independently so the two bars can differ and so a demotion resets
-- both without ambiguity about which streak a count belonged to.
--
-- last_outcome GAINS 'ratified' — PROVENANCE, NOT A VERIFICATION. Ratification is an operator act, not a run:
-- a class enters the ladder at approve with last_outcome='ratified' and has verified NOTHING yet. Recording
-- it as 'verified_clean' would assert a run that never happened (the exact honesty defect 0040 fixed for
-- 'seeded'). Like OutcomeSeeded it never promotes and never demotes.
ALTER TABLE policy_graduation DROP CONSTRAINT IF EXISTS policy_graduation_level_check;
ALTER TABLE policy_graduation ADD CONSTRAINT policy_graduation_level_check
  CHECK (level IN ('approve', 'auto_notice', 'auto'));

ALTER TABLE policy_graduation DROP CONSTRAINT IF EXISTS policy_graduation_last_outcome_check;
ALTER TABLE policy_graduation ADD CONSTRAINT policy_graduation_last_outcome_check
  CHECK (last_outcome IN ('unverified', 'verified_clean', 'deviated', 'seeded', 'ratified'));

ALTER TABLE policy_graduation ADD COLUMN IF NOT EXISTS notice_run_count int NOT NULL DEFAULT 0
  CHECK (notice_run_count >= 0);

COMMENT ON COLUMN policy_graduation.notice_run_count IS
  'Consecutive verified-clean runs accumulated AT auto_notice toward the silent rung (spec/028 REQ-2808). Separate from clean_run_count so the two climbs cannot be confused and a demotion resets both unambiguously.';

-- EXACTLY-ONCE PROMOTION CREDIT (REQ-2804). One stopped guest raises four alert rules; a paused-and-resumed
-- session re-runs. Without a key, ONE incident could advance a class four rungs' worth of streak — the same
-- 4x-credit lesson the occurrence journal paid for, applied to the ladder. Consulted BEFORE any streak
-- increment: credit is claimed by (op_class, external_ref) or it is not claimed at all.
CREATE TABLE graduation_credit (
  op_class     text        NOT NULL CHECK (length(btrim(op_class)) > 0),
  external_ref text        NOT NULL CHECK (length(btrim(external_ref)) > 0),
  outcome      text        NOT NULL DEFAULT '',
  credited_at  timestamptz NOT NULL DEFAULT now(),
  schema_version int       NOT NULL DEFAULT 1 CHECK (schema_version > 0),
  UNIQUE (op_class, external_ref)
);

CREATE INDEX graduation_credit_time ON graduation_credit (credited_at);

COMMENT ON TABLE graduation_credit IS
  'Exactly-once ladder credit by (op_class, external_ref) — spec/028 REQ-2804. Consulted before any streak increment so one incident cannot manufacture a promotion through alert-rule multiplicity.';

-- Credit that can be rewritten is not credit.
REVOKE UPDATE, DELETE ON graduation_credit FROM tg_runtime;
