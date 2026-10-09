ALTER TABLE co_occurrence_host
  DROP COLUMN IF EXISTS recovery_sum,
  DROP COLUMN IF EXISTS recovery_count;
