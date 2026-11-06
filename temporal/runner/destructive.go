package runner

import (
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
)

// destructiveOf is the server-side destructiveness the classifier trusts over the model's declared class
// (TG-641, owner-ruled 2026-11-06). The ACTION, the command that would run, keeps the full destructive-op
// pattern. The RATIONALE is prose and counts only when it spells out a destructive command: a sentence ABOUT
// a shutdown ("no evidence of a planned shutdown") is not a shutdown, and it held four correct start-guests in
// the G3 soak. The value is pure data handed to the classify activity, so the workflow needs no version guard
// for it (the same reasoning as the other classify inputs in workflow.go).
func destructiveOf(a manifest.Action, rationale string) bool {
	return safety.IsDestructiveOp(actionSafetyParts(a)...) || safety.IsDestructiveProse(rationale)
}
