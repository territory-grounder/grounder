-- 0089 down: restore the 8 KiB schema body cap and drop the artifact-class column.
--
-- NOTE (deliberate, documented): the cap restore FAILS if any body over 8192 bytes exists —
-- Postgres validates every row on ADD CONSTRAINT. That is the honest behavior, not a defect: a
-- store already carrying prompt/runbook/rubric-sized bodies cannot silently return to a skill-only
-- schema; an operator must first decide what happens to those rows (delete or truncate them), which
-- is a data decision no down-migration may make on its own.
ALTER TABLE skill_version DROP CONSTRAINT skill_version_body_check;
ALTER TABLE skill_version ADD CONSTRAINT skill_version_body_check
  CHECK (octet_length(body) BETWEEN 1 AND 8192);
ALTER TABLE skill
  DROP COLUMN artifact_class;
