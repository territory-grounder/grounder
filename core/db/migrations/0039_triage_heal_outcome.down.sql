-- 0039 down: drop the additive heal-outcome columns.
ALTER TABLE session_triage
    DROP COLUMN confirmed_clear,
    DROP COLUMN mutated;
