package runner

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/risk"
	"github.com/territory-grounder/grounder/core/safety"
)

// THE AUTONOMOUS FALLBACK APPROVER (spec/012 REQ-1127, TG-559). Owner ruling 2026-09-13: "there should be no
// voting --> TG must act autonomously".
//
// CONSTITUTION §4.3 makes the human circuit-breaker an approver GRAPH that ends in a fallback approver, and says
// what the graph is for: act on the safe-and-recoverable, page on the impactful-but-reversible, hold ONLY the
// irreversible or security event. Nobody attends this deployment's poll lane (standing ruling, TG-173), so a
// POLL_PAUSE was a VoteWait-long park that ended as `human:timeout` = DENY — measured 2026-09-13: 251 timeouts
// against 119 approvals, nearly all of those cast by drill sessions. So when TG_APPROVAL_RESOLVER=autonomous the
// graph's last node is TG itself, and it decides the moment the band is known.
//
// HOW IT DECIDES — by asking the classifier, not by reading the reason it recorded. The classifier returns at
// the FIRST rule that polls, so `poll_reason` names one reason and hides the rest: a canary pin is checked
// before actor attribution, and the un-graduated-class rule before the prediction, deviation, novelty and
// high-risk rules. A table keyed on the recorded reason would therefore ACT on "canary-policy-pinned" while a
// person's deliberate change to the same host sat unrecorded behind it. Instead the decision is COUNTERFACTUAL:
// clear exactly the three inputs whose only content is "no human has looked yet" —
//
//   - CanaryPinned      a deployment asked for a first look at a staged mutation;
//   - UngraduatedClass  the op-class has not yet climbed the earned-autonomy ladder;
//   - NovelIncident     the incident shape is new to TG —
//
// and classify again with the SAME classifier. If the action no longer polls, a missing first look was the
// whole reason to ask, and the fallback approver ACTS. If it still polls, a real reason remains — irreversible
// or destructive, stateful, a security signal, a person's deliberate change ("leave it, tell me"), a declared
// pack floor, or doubtful grounding (no prediction, a recent deviation, prose naming another host, unbound
// evidence) — and the fallback approver HOLDS, naming that reason. The owner's three answers are on TG-559.
//
// Reusing the classifier is the point: the rules cannot drift between the gate and its fallback, and a rule
// added to the classifier later is held by default without anyone remembering to extend a table here.
//
// What this does NOT change: the resolution only replaces the WAIT. An ACT still travels the whole interceptor
// chain with Approved=true — mode chokepoint, never-auto floor, allowlists, evidence binding, necessity and
// breaker all still judge it, and any of them still refuses.

// AutonomousApproverPrincipal is the voter identity the fallback approver records on the governance ledger. It
// can never collide with a human login, so the ledger always says WHO released or held an action.
const AutonomousApproverPrincipal = "tg:fallback-approver"

// The classification signals the resolution rides on. They are written by ClassifyActivity — an activity, so the
// decision is recorded in workflow history and a replay reads the same answer — and read by the workflows.
const (
	signalAutonomousResolution = "autonomous_resolution" // "act" | "hold"
	signalAutonomousKey        = "autonomous_key"        // the poll reason that decided
	signalAutonomousWhy        = "autonomous_why"        // one human-readable sentence for the notice
)

// AutonomousResolution is the fallback approver's decision on one POLL_PAUSE. The zero value (Resolved=false)
// means "not resolved here" — the human vote path applies exactly as before.
type AutonomousResolution struct {
	Resolved bool   // the fallback approver decided: no poll is posted and no vote is awaited
	Approve  bool   // true ⇒ proceed to execute with Approved=true; false ⇒ hold (stand down without mutation)
	Key      string // the poll reason that decided (the one that asked, for an ACT; the one that remains, for a HOLD)
	Why      string // one sentence for the notice
}

// resolvePollAutonomously decides one classified proposal by counterfactual classification (see the file
// comment). gi is the exact input the classifier judged and polled is its decision; neither is modified. A
// decision that did not poll needs no approver and is returned unresolved.
func resolvePollAutonomously(gi risk.GatedInput, polled risk.Decision) AutonomousResolution {
	if polled.Band != safety.BandPollPause {
		return AutonomousResolution{}
	}
	cf := gi
	cf.CanaryPinned, cf.UngraduatedClass, cf.NovelIncident = false, false, false
	// A private copy of the signals: the classifier WRITES into the map it is handed (poll_reason and friends),
	// and gi.Signals can be the very map the recorded decision carries — sharing it would overwrite the reason
	// the audit trail shows with the counterfactual's.
	cf.Signals = make(map[string]string, len(gi.Signals))
	for k, v := range gi.Signals {
		cf.Signals[k] = v
	}
	delete(cf.Signals, "poll_reason")

	asked := polled.Signals["poll_reason"]
	again := risk.Classify(cf)
	if again.Band == safety.BandPollPause {
		remains := again.Signals["poll_reason"]
		if remains == "" {
			remains = "unrecorded"
		}
		return AutonomousResolution{
			Resolved: true,
			Key:      remains,
			Why:      fmt.Sprintf("Held: %q still requires caution without a human's first look, so TG does not act", remains),
		}
	}
	return AutonomousResolution{
		Resolved: true,
		Approve:  true,
		Key:      asked,
		Why:      fmt.Sprintf("Acting: %q was the only reason to ask — without it the action classifies %s", asked, again.Band),
	}
}

// resolveRollbackAutonomously decides the manual-rollback poll. That poll's only content is a SECOND
// confirmation of an undo an operator already requested, and SealRollbackActivity has re-validated the inverse
// as reversible before this is reached; an inverse that is not reversible is held.
func resolveRollbackAutonomously(inverseReversible bool) AutonomousResolution {
	if !inverseReversible {
		return AutonomousResolution{Resolved: true, Key: "manual-rollback-irreversible", Why: "Held: the inverse action is not reversible"}
	}
	return AutonomousResolution{
		Resolved: true, Approve: true, Key: "manual-rollback",
		Why: "Acting: an operator requested this undo and its inverse re-validated as reversible",
	}
}

// recordAutonomousResolution writes a resolution onto the classification signals (ClassifyActivity side).
func recordAutonomousResolution(d *risk.Decision, r AutonomousResolution) {
	if !r.Resolved {
		return
	}
	// A FRESH map, never a write into the one the decision carries: that map is shared with the classifier's
	// input and with the risk-audit row already appended to the hash-chained ledger, and mutating it after the
	// append would change a recorded entry out from under its hash.
	signals := make(map[string]string, len(d.Signals)+3)
	for k, v := range d.Signals {
		signals[k] = v
	}
	signals[signalAutonomousResolution] = "hold"
	if r.Approve {
		signals[signalAutonomousResolution] = "act"
	}
	signals[signalAutonomousKey] = r.Key
	signals[signalAutonomousWhy] = r.Why
	d.Signals = signals
}

// autonomousResolutionOf reads back the resolution the classify activity recorded (workflow side — a pure read
// of recorded history). Anything other than exactly "act" or "hold" is unresolved: the human path applies.
func autonomousResolutionOf(d risk.Decision) AutonomousResolution {
	r := AutonomousResolution{Key: d.Signals[signalAutonomousKey], Why: d.Signals[signalAutonomousWhy]}
	switch d.Signals[signalAutonomousResolution] {
	case "act":
		r.Resolved, r.Approve = true, true
	case "hold":
		r.Resolved = true
	default:
		return AutonomousResolution{}
	}
	return r
}

// autonomousLedgerDecision is the governance-ledger decision string for a resolution — the fallback approver's
// counterpart of `human:approve` / `human:deny`, carrying the reason that decided.
func autonomousLedgerDecision(r AutonomousResolution) string {
	if r.Approve {
		return "fallback:autonomous-approve:" + r.Key
	}
	return "fallback:autonomous-hold:" + r.Key
}

// autonomousNoticeBody renders the notice the fallback approver sends INSTEAD of a poll: what TG decided and
// why. It carries no vote instructions, because there is nothing to vote on.
func autonomousNoticeBody(alertRule, host, op, opClass, externalRef string, r AutonomousResolution) string {
	verb := "HOLDING — no action taken"
	if r.Approve {
		verb = "ACTING"
	}
	return fmt.Sprintf("[TG DECIDED: %s] %s on %s — %s (%s). %s. No vote needed. ref=%s",
		verb, alertRule, host, op, opClass, r.Why, externalRef)
}

// rollbackAutonomousDecision records the fallback approver's decision on a manual-rollback poll in place of
// rollbackVoteWait. For an ACT the record is fail-closed: an approval that could not be made durable releases
// nothing.
func rollbackAutonomousDecision(ctx workflow.Context, a *Activities, in RollbackInput, seal SealRollbackResult, r AutonomousResolution) (approved bool, vote string) {
	recCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	var rec RecordVoteResult
	err := workflow.ExecuteActivity(recCtx, a.RecordVoteActivity, RecordVoteInput{
		Decision: autonomousLedgerDecision(r), ActionID: seal.InverseActionID,
		ExternalRef: in.RollbackExternalRef, Voter: AutonomousApproverPrincipal,
	}).Get(ctx, &rec)
	switch {
	case !r.Approve:
		return false, "autonomous-held"
	case err != nil:
		return false, "autonomous-unrecorded"
	default:
		return true, "autonomous-approved"
	}
}
