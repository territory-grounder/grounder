-- 0057 down: drop the terminal-decision-tier column. model_tier (0027) is untouched — it never carried
-- this fact, which is why 0057 exists.
ALTER TABLE session_triage DROP COLUMN IF EXISTS decision_model_tier;
