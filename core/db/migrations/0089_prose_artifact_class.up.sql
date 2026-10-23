-- 0089: the prose-artifact CLASS MODEL on the ONE skill store (spec/014 REQ-1315/1316, ADR-0017,
-- TG-470, epic TG-114 C-1).
--
-- Every LLM-facing prose artifact — agent skills, the base prompt's trialable half, runbooks, the
-- judge-rubric mirror — becomes a row on the EXISTING store, discriminated by a closed artifact_class
-- vocabulary. Never a parallel prose_artifact table: the store's two structural invariants (one
-- production version per name, one active trial per name) are exactly what every prose class needs,
-- and re-implementing them is how the predecessor's supersede logic drifted (0009's founding lesson).
--
-- LIVE-SAFETY: this migration performs ZERO row rewrites. ADD COLUMN with a constant default is a
-- catalog-only change on PostgreSQL 11+ (attmissingval — existing rows are not touched, its CHECK is
-- satisfied by that constant for all of them), and the body-cap swap below only VALIDATES existing
-- rows with a read scan of skill_version (dozens of rows; every one already ≤ 8192 passes the wider
-- bound trivially). skill_trial / skill_trial_assignment are untouched — no FK change, no data
-- movement — so the three live crons and any ACTIVE trial see nothing beyond the brief
-- ACCESS EXCLUSIVE catalog update, and every stored body survives byte-for-byte.
ALTER TABLE skill
  ADD COLUMN artifact_class text NOT NULL DEFAULT 'skill'
  CHECK (artifact_class IN ('skill', 'prompt', 'runbook', 'rubric'));

-- Widen the SCHEMA ceiling to the LARGEST class's cap: runbook/rubric bodies reach well past 8 KiB
-- (predecessor runbooks measure up to 15.6 KB). The PER-CLASS caps — skill 8 KiB, prompt 16 KiB,
-- runbook/rubric 32 KiB — are DOMAIN-layer law (core/skillstore MaxBodyBytes), enforced at the write
-- gate (ValidateDraft) and RE-CHECKED at composition (agent/skills.NewFromStore), so widening the
-- schema for runbooks cannot silently relax the skill cap (REQ-1316). The dropped name is the
-- auto-generated one Postgres gave 0009's inline column CHECK (<table>_<column>_check — verified on
-- the live schema: skill_version_body_check); it is re-created under the SAME name so \d output and
-- any tooling keyed on it stay stable.
ALTER TABLE skill_version DROP CONSTRAINT skill_version_body_check;
ALTER TABLE skill_version ADD CONSTRAINT skill_version_body_check
  CHECK (octet_length(body) BETWEEN 1 AND 32768);
