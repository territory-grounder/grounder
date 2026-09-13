-- 0058 down: drop the wall-clock decision-latency column. step_count (0037) is untouched — it carries the
-- STEPS half of the split axis (A6a), which is a different measurement and remains the merge gate.
ALTER TABLE session_triage DROP COLUMN IF EXISTS decision_ms;
