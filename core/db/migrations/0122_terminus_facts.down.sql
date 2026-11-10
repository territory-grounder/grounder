-- 0122 down: drop the terminus record table and the additive terminus-facts columns (TG-662).
DROP TABLE IF EXISTS terminus_record;
ALTER TABLE session_triage
    DROP CONSTRAINT IF EXISTS session_triage_causality_chk,
    DROP COLUMN causality_evidence,
    DROP COLUMN causality,
    DROP COLUMN terminus_facts_at,
    DROP COLUMN collateral_observed,
    DROP COLUMN confirmed_clear_at;
