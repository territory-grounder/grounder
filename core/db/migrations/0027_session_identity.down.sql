-- 0027 down: drop the additive session-identity provenance columns.
ALTER TABLE session_triage
    DROP COLUMN prompt_version,
    DROP COLUMN seed_hash,
    DROP COLUMN model_tier;
