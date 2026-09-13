-- 0028 down: drop the additive external_ref correlation column.
ALTER TABLE policy_decision
    DROP COLUMN external_ref;
