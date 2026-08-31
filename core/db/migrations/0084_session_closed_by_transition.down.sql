-- 0084 down: drop the recovery-closer audit link. Reversible and lossless for the schema; a re-apply of the
-- up migration re-adds the column as all-NULL (the reconciler back-fills it only on NEW recoveries, never
-- retroactively), which is the same honest "not known" the additive column already means.
ALTER TABLE session_triage
    DROP COLUMN closed_by_transition_id;
