package main

import (
	"fmt"
	"log"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/policy"
)

// graduationTransitionRecorder turns a ladder transition into a governance-ledger row (TG-666 part 4):
// `graduation:<promote|demote|ceiling-held>:<op_class>`, with from → to, the outcome that moved it and the ladder's
// own reason. A failed append is logged, never fatal: the rung change already landed in the durable store.
func graduationTransitionRecorder(appendDecision func(audit.GovDecision) error) func(policy.RecordResult) {
	return func(r policy.RecordResult) {
		kind := "promote"
		switch {
		case r.Demoted:
			kind = "demote"
		case r.StreakReset:
			kind = "streak-reset"
		case r.CeilingHeld:
			kind = "ceiling-held"
		}
		reason := fmt.Sprintf("%s → %s on a %s run (%s)", r.From, r.To, r.Outcome, r.Reason)
		if r.Operator {
			reason = fmt.Sprintf("%s → %s by an operator (%s)", r.From, r.To, r.Reason)
		}
		d := audit.GovDecision{
			Decision: "graduation:" + kind + ":" + r.OpClass,
			Reason:   reason,
			ActionID: "graduation:" + r.OpClass,
			// An OPERATOR's demotion or streak reset withholds autonomy on purpose (TG-666 part 3). Automatic run-outcome
			// transitions stay Withheld=false, as before: the wiki roster counts withheld rows as "TG declined to act".
			Withheld: r.Operator && (r.Demoted || r.StreakReset),
		}
		if err := appendDecision(d); err != nil {
			log.Printf("graduation: transition %s for %s landed but its ledger row failed: %v (TG-666)", kind, r.OpClass, err)
		}
	}
}

// wireGraduationTransitions binds the ladder's transition hook to the governance ledger (TG-666 part 4): every rung
// change that durably landed becomes a hash-chained row. Until this hook no ladder transition was ever recorded, so
// how a class reached its rung (F22) could not be reconstructed from the ledger.
func wireGraduationTransitions(l *policy.Ladder, ledger *audit.Ledger) {
	l.WithTransitionHook(graduationTransitionRecorder(func(d audit.GovDecision) error {
		_, err := ledger.Append(d)
		return err
	}))
}
