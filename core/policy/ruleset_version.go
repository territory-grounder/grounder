package policy

// BundleVersion is the deterministic, NON-SECRET content fingerprint of an operator rule set — the "bundle
// version" recorded on every decision (REQ-1522) and the JOIN KEY the decision tracer uses to resolve a past
// policy_decision to the EXACT immutable ruleset document in force when it decided (spec/020 T-020-6, REQ-2018).
//
// It is the single exported entry point to the (unexported) bundleVersion hash, so the persistence layer
// (core/db.PolicyRulesetStore / RulesetVersionStore) derives a ruleset's version through the ONE canonical
// implementation rather than duplicating the canonicalization — the version a decision is stamped with and the
// version its ruleset is archived under are then always computed the same way.
func BundleVersion(rs RuleSet) string { return bundleVersion(rs) }
