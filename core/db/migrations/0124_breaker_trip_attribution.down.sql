-- 0124 down: drop the TG-665 trip-attribution and compensation columns.
ALTER TABLE mutation_breaker_state
    DROP COLUMN compensation_admitted_at,
    DROP COLUMN compensation_action_id,
    DROP COLUMN trip_cause,
    DROP COLUMN tripped_by_action_id;
