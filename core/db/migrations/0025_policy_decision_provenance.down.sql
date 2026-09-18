-- 0025 down: drop the additive provenance columns.
ALTER TABLE policy_decision
    DROP COLUMN bundle_version,
    DROP COLUMN matched_rules,
    DROP COLUMN reason;
