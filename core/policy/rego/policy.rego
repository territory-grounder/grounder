# FIXED, AUDITED policy evaluator — spec/015 REQ-1503 / REQ-1504.
#
# This is the ONE Rego module the engine compiles (it is `go:embed`-ed into eval.go and prepared once,
# in-process, via the OPA Go SDK — no sidecar, no network, distroless-safe). Operators SHALL NEVER author
# Rego: their policy enters ONLY as `input.rules` DATA. There is no path from operator input to evaluator
# logic — the logic below is fixed and audited (REQ-1503, INV-06).
#
# The estate-object-model + policy-dimension MATCH of each rule is computed in Go against the ONE shared
# selector grammar (core/credential.Match — the same primitive the credential engine keys off, REQ-1605),
# and delivered here as the pre-computed boolean `input.rules[_].matched`. This module deliberately does
# NOT reimplement host/glob/group/device-class matching — that would be a second, divergent selector
# grammar. This module owns exactly ONE thing: the deny-overrides COMBINATION effect (REQ-1504).
package tg.policy

import rego.v1

# The multiset of verdicts carried by rules that matched the action.
matched_verdicts contains v if {
	some r in input.rules
	r.matched == true
	v := r.verdict
}

any_deny if "deny" in matched_verdicts

any_approve if "approve" in matched_verdicts

any_auto if "auto" in matched_verdicts

# Deny-overrides ladder (REQ-1504): deny beats everything, then approve, then auto; the bodies are mutually
# exclusive so exactly one fires and the result is order-independent (a deny can never be shadowed by rule
# ordering or by a more-permissive matching rule).
verdict := "deny" if any_deny

verdict := "approve" if {
	not any_deny
	any_approve
}

verdict := "auto" if {
	not any_deny
	not any_approve
	any_auto
}

# Fail-closed default (REQ-1506 / REQ-1507 / REQ-1517): when NO rule matches (or only non-verdict data is
# present), route to a human — `approve`. Never a silent `auto` without an explicit qualifying allow rule,
# and never a hard `deny` (that would violate the operator's warn-don't-block invariant for actions they
# have written no deny for). This mirrors the design's "highest-authority allow/approve, else route to a
# human vote".
verdict := "approve" if {
	not any_deny
	not any_approve
	not any_auto
}

# Projection helpers for the structured Decision (non-secret rule-id provenance, REQ-1518 audit input).
matched_ids := [r.id | some r in input.rules; r.matched == true]

deny_ids := [r.id | some r in input.rules; r.matched == true; r.verdict == "deny"]

winning_ids := [r.id | some r in input.rules; r.matched == true; r.verdict == verdict]

# The single decision object the engine reads (queried as data.tg.policy.result).
result := {
	"verdict": verdict,
	"matched_ids": matched_ids,
	"deny_ids": deny_ids,
	"winning_ids": winning_ids,
}
