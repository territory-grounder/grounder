-- 0040: widen policy_graduation.last_outcome to accept the 'seeded' provenance (TG-183).
--
-- SeedDefaults places curated reversible op-classes at level=auto as the out-of-box baseline. Before this it
-- stamped last_outcome='verified_clean' on those seeded rows — asserting a VERIFICATION that never happened
-- (no run, no governance_ledger earn event). last_outcome is a durable, operator-facing record, so a seeded
-- class must be DISTINGUISHABLE from an EARNED one. 'seeded' is a non-verified, non-promoting provenance label
-- (core/policy.OutcomeSeeded): it grants no autonomy on its own; only a real OutcomeVerifiedClean run does.
--
-- Forward-only, additive: drop the 0019 inline CHECK and re-add it including 'seeded'. No data is rewritten by
-- this schema migration; the one-time correction of any live seeded-but-mislabelled rows is a separate,
-- governance-ledgered operational step (it depends on per-deployment earn history, which SQL cannot infer).
ALTER TABLE policy_graduation DROP CONSTRAINT IF EXISTS policy_graduation_last_outcome_check;
ALTER TABLE policy_graduation ADD CONSTRAINT policy_graduation_last_outcome_check
  CHECK (last_outcome IN ('unverified', 'verified_clean', 'deviated', 'seeded'));
