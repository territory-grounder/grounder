-- Reverse 0040: normalize any 'seeded' rows to the fail-safe 'unverified' (a value the narrower CHECK accepts
-- and which, like 'seeded', grants no autonomy), then restore the pre-0040 constraint.
UPDATE policy_graduation SET last_outcome = 'unverified' WHERE last_outcome = 'seeded';
ALTER TABLE policy_graduation DROP CONSTRAINT IF EXISTS policy_graduation_last_outcome_check;
ALTER TABLE policy_graduation ADD CONSTRAINT policy_graduation_last_outcome_check
  CHECK (last_outcome IN ('unverified', 'verified_clean', 'deviated'));
