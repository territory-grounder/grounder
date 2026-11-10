-- 0123 down: PostgreSQL cannot remove a value from an enum in place. A rollback that must drop 'indeterminate'
-- recreates the type: rows carrying the value have to be deleted or re-labelled first (they are append-only
-- evidence, so the honest down is a no-op that leaves the value in place and restores the column comment).
COMMENT ON COLUMN action_execution.verdict IS
    'the fresh mechanical verdict for THIS execution, or NULL when the post-state was unobservable';
