package runner

import (
	"github.com/territory-grounder/grounder/core/manifest"
)

// action_params_safety.go — THE STATEFUL/DESTRUCTIVE FLOOR MUST SEE THE PARAMS (TG-146 A3).
//
// safety.IsStatefulWorkload's own doc says it reports "whether any of the given strings (a target host,
// an op, ITS PARAMS) names a stateful workload". The classifier call site passed only Target, Op and
// OpClass — the params were never handed to it.
//
// THE CONSEQUENCE, and why it is not merely cosmetic. A proposal like
//
//	Target:  dc1app01           (no stateful token)
//	Op:      restart-service        (no stateful token)
//	OpClass: restart-service        (no stateful token)
//	Params:  {"unit": "mariadb.service"}   <-- the ONLY place the database appears
//
// classifies as Stateful=false, so `stateful-workload-mutation` never clamps it and the band is RECORDED
// as auto. The audit row, the graduation credit and the operator's view all say a database restart was an
// ordinary reversible action.
//
// It was described as "1-deep" because the ssh effect leaf DOES read the unit
// (modules/actuation/ssh/mutate.go: IsStatefulWorkload(unit, oc)). Measured 2026-08-06, that depth is one
// lane out of five: awxjob, kubernetes, mcp and proxmox have NO stateful check of their own, so for those
// lanes the classifier is the only line of defence and it could not see the workload it was defending.
//
// OVER-MATCHING IS THE INTENDED DIRECTION. Passing every param VALUE means a free-text param that happens
// to contain "database" clamps to POLL_PAUSE. core/safety states the trade explicitly for this very
// predicate: "a false positive costs one extra human review, a false negative costs a database."
//
// Keys are sorted so this is obviously deterministic in workflow code. The predicates OR over their parts,
// so the RESULT is order-independent either way — the sort is for reviewability, not correctness.
func actionSafetyParts(a manifest.Action) []string {
	// The derivation is SHARED with the adapter floor (manifest.Action.SafetyParts, TG-146 A3 second
	// half): one source, two depths, so the classify-time floor and the adapter floor cannot drift.
	return a.SafetyParts()
}
