ALTER TABLE commit_confirm
    DROP COLUMN IF EXISTS forward_band,
    DROP COLUMN IF EXISTS forward_approved,
    DROP COLUMN IF EXISTS alert_rule;
