-- 0025: persist the policy engine's already-computed decision PROVENANCE on policy_decision
-- (spec/020 T-020-2, REQ-2004; design-wisdom #12 harden-policy-decision-surface). The engine composes
-- bundle_version + the FULL matched-rules list + a human-readable reason in memory (core/policy DecisionAudit,
-- REQ-1522) but the writer dropped all three at the DB boundary, so a persisted decision could not be joined
-- back to the exact ruleset + rules that produced it, and the console packet-tracer had only the winning rule.
-- These three additive, defaulted, NON-SECRET columns close the gap: bundle_version is the content-derived
-- ruleset fingerprint, matched_rules is the deny-overrides provenance projected to {id, verdict} per rule
-- (never argv/host/credential; INV-13), and reason is the composed-decision explanation. Backward-compatible:
-- pre-fix rows read as an empty bundle_version, an empty matched_rules list, and an empty reason.
ALTER TABLE policy_decision
    ADD COLUMN bundle_version text  NOT NULL DEFAULT '',
    ADD COLUMN matched_rules  jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN reason         text  NOT NULL DEFAULT '';
