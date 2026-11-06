DROP TABLE IF EXISTS graduation_credit;

ALTER TABLE policy_graduation DROP COLUMN IF EXISTS notice_run_count;

ALTER TABLE policy_graduation DROP CONSTRAINT IF EXISTS policy_graduation_last_outcome_check;
ALTER TABLE policy_graduation ADD CONSTRAINT policy_graduation_last_outcome_check
  CHECK (last_outcome IN ('unverified', 'verified_clean', 'deviated', 'seeded'));

ALTER TABLE policy_graduation DROP CONSTRAINT IF EXISTS policy_graduation_level_check;
ALTER TABLE policy_graduation ADD CONSTRAINT policy_graduation_level_check
  CHECK (level IN ('approve', 'auto'));
