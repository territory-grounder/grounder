package runner

import (
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/modules/actuation/ssh"
)

// destructiveOf is the server-side destructiveness the classifier trusts over the model's declared class
// (TG-641, owner-ruled 2026-11-06). The ACTION, the command that would run, keeps the full destructive-op
// pattern. The RATIONALE is prose and counts only when it spells out a destructive command: a sentence ABOUT
// a shutdown ("no evidence of a planned shutdown") is not a shutdown, and it held four correct start-guests in
// the G3 soak. The value is pure data handed to the classify activity, so the workflow needs no version guard
// for it (the same reasoning as the other classify inputs in workflow.go).
//
// A mutation of a NEVER-TOUCH subject is destructive too (TG-653): see neverTouchSubject.
func destructiveOf(a manifest.Action, rationale string) bool {
	return safety.IsDestructiveOp(actionSafetyParts(a)...) || safety.IsDestructiveProse(rationale) || neverTouchSubject(a)
}

// neverTouchSubject reports whether the action names, as its unit or container, a subject on the SSH leaf's
// never-touch list: the access, network, container-runtime, hypervisor, cluster, routing, VPN and storage
// planes, whose restart can sever TG from the host, the host from the estate, or a hypervisor from its
// cluster (ssh.NeverTouchUnit, the one implementation, not a copy).
//
// WHY AT CLASSIFY TIME. On 2026-11-07 TG proposed `restart-service {"unit":"pve-cluster.service"}` on a
// Proxmox hypervisor. Classification, the autonomous fallback approver and the policy bundle all passed it,
// and only the execute-time never-touch check in the leaf refused it. That is defence ONE layer deep: if the
// leaf's list ever drifted, or another leaf served the action, nothing upstream would stop it. Read here, the
// same predicate makes the classifier poll (server-destructive), and the fallback approver HOLDS it, because
// clearing a missing first look does not clear a destructive op.
//
// Consequence, stated: the leaf honours an operator's EXPLICIT allowlist entry for a never-touch unit (listed
// mode never consults the list). Classify-time, such a grant is now held. No live allowlist names one
// (2026-11-07: units nginx and apache2; containers mealie, ghostfolio and actualbudget).
func neverTouchSubject(a manifest.Action) bool {
	for _, k := range []string{"unit", "container"} {
		if v, ok := a.Params[k]; ok && ssh.NeverTouchUnit(v) {
			return true
		}
	}
	return false
}
