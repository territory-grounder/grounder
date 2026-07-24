// Package reconcile implements Territory Grounder's band-aware close-out lane: it drives a
// genuinely-finished session to a terminal decision and closes an incident ONLY on a confirmed clear —
// never on the acting agent's asserted success.
//
// Provenance: [F] spec/003 (BEH-3), the predecessor reconcile-completed-sessions.py band-aware
// close-out, re-expressed under the typed spine · [O] INV-11 (a close cites an orchestrator-captured
// confirmation, not agent free-text), INV-12 · [R] paradigm-rule 1 (single-org; per-incident accounting).
package reconcile

import (
	"github.com/territory-grounder/grounder/core/safety"
)

// TicketState is the tracker ticket state a session transitions to at close-out.
type TicketState string

const (
	TicketToVerify TicketState = "To Verify" // left open — not a silent close
	TicketDone     TicketState = "Done"
)

// ResolutionType is the recorded outcome class (REQ-203).
type ResolutionType string

const (
	ResAutoResolved   ResolutionType = "auto_resolved"
	ResHumanResolved  ResolutionType = "human_resolved"
	ResEscalated      ResolutionType = "escalated"
	ResDeferred       ResolutionType = "deferred"
	ResPollUnanswered ResolutionType = "poll_unanswered" // an orphaned poll — archived + a re-check scheduled (REQ-206)
)

// FinishedSession is the typed input the reconciler evaluates. ConfirmedClear is true only when an
// orchestrator-captured ToolResult or an independent post-condition check confirmed the alert condition
// actually cleared — it is never set from the agent's asserted success (INV-11).
type FinishedSession struct {
	ExternalRef       string
	SessionID         string
	ActionID          string
	Band              safety.Band
	HasVerdict        bool
	Verdict           safety.Verdict
	ConfirmedClear    bool // an orchestrator-captured confirmation the condition cleared
	HasTerminalResult bool // false ⇒ crash / timeout / indeterminate
	// PollUnanswered is true when this was a POLL_PAUSE session whose approval poll aged out with no human
	// answer — an ORPHANED poll. The incident is unresolved and must not be dropped: it schedules a delayed
	// re-check (REQ-206, the IFRNLLEI01PRD-1536 fix — a 90→100% disk poll that went unanswered and silently
	// worsened). ReCheckAttempts is the number of re-checks already scheduled for it.
	PollUnanswered  bool
	ReCheckAttempts int
	// Executed is true when this session actually RAN a mutating remediation (a committed action prediction
	// reached the actuation interceptor) — i.e. Phase-2, mutation ON. It is false for a read-only / confirm-close
	// session (Phase 0/1, where nothing executes). An executed remediation may reach Done ONLY on a clean
	// MATCH verdict; an executed session whose async blast-radius verdict has not yet landed is HELD, never
	// auto-closed on the alert clearing alone.
	Executed bool
}

// ReconcileDecision is the reconciler's output.
type ReconcileDecision struct {
	Close      bool
	Ticket     TicketState
	Resolution ResolutionType
	Reason     string
	// ScheduleReCheck instructs the reconcile activity to enqueue an escalation.Controller.ScheduleReCheck
	// row for an orphaned poll (REQ-206). ReCheckAttempts carries the prior attempt count forward.
	ScheduleReCheck bool
	ReCheckAttempts int
}

// Reconcile is the deterministic close-out decision, ordered most-conservative-first:
//
//  1. No terminal result (crash/timeout/indeterminate) → leave the incident OPEN, transition to
//     To Verify — never a silent close (REQ-204).
//  2. A deviation verdict → never auto-close; route to To Verify (spec/002 REQ-104 composed here).
//  3. The alert condition was NOT confirmed clear → do not close on asserted success; To Verify (REQ-201).
//  4. Confirmed clear + a terminal result: a band-AUTO / AUTO_NOTICE session transitions its ticket to
//     Done as auto_resolved (REQ-202); otherwise Done as human_resolved.
func Reconcile(s FinishedSession) ReconcileDecision {
	// An orphaned poll (a POLL_PAUSE the human never answered before it aged out) is NEVER silently dropped:
	// the incident is unresolved, so it archives as poll_unanswered AND schedules a delayed re-check against
	// the live condition (REQ-206). The re-check re-enters execution only through the authenticated gate.
	if s.PollUnanswered {
		return ReconcileDecision{
			Close: false, Ticket: TicketToVerify, Resolution: ResPollUnanswered,
			ScheduleReCheck: true, ReCheckAttempts: s.ReCheckAttempts,
			Reason: "orphaned poll — archive poll_unanswered and schedule a re-check",
		}
	}
	if !s.HasTerminalResult {
		return ReconcileDecision{Close: false, Ticket: TicketToVerify, Resolution: ResDeferred, Reason: "no terminal result — leave open"}
	}
	// A non-clean verdict never auto-closes: a deviation, a PARTIAL (a predicted host fired an unpredicted
	// rule — the outcome diverged from the model), or any verdict the verifier did not validly produce is
	// routed to To Verify for human confirmation. Only a clean MATCH may auto-close (REQ-104, P0-8) — a
	// write-action must not silently close before a clean blast-radius verdict.
	if s.HasVerdict && s.Verdict != safety.VerdictMatch {
		reason := "non-match verdict — human-confirmed close"
		if s.Verdict == safety.VerdictDeviation {
			reason = "deviation — never auto-close"
		} else if s.Verdict == safety.VerdictPartial {
			reason = "partial verdict — never silently auto-close"
		}
		return ReconcileDecision{Close: false, Ticket: TicketToVerify, Resolution: ResEscalated, Reason: reason}
	}
	if !s.ConfirmedClear {
		return ReconcileDecision{Close: false, Ticket: TicketToVerify, Resolution: ResDeferred, Reason: "condition not confirmed clear — no close on asserted success"}
	}
	if s.Band == safety.BandAuto || s.Band == safety.BandAutoNotice {
		// An AUTO session that EXECUTED a remediation reaches Done ONLY on a clean MATCH verdict. If the async
		// verifier has not yet written the verdict (HasVerdict=false — the non-match case was already routed to
		// To Verify above), HOLD rather than close an executed write-action on the alert clearing alone: a
		// remediation that in fact cascaded (would verify partial/deviation) must not silently close before its
		// blast-radius verdict lands. A read-only / confirm-close AUTO session (nothing executed) closes on the
		// confirmed clear.
		if s.Executed && !(s.HasVerdict && s.Verdict == safety.VerdictMatch) {
			return ReconcileDecision{Close: false, Ticket: TicketToVerify, Resolution: ResEscalated, Reason: "executed remediation without a clean match verdict — hold for verification"}
		}
		return ReconcileDecision{Close: true, Ticket: TicketDone, Resolution: ResAutoResolved, Reason: "confirmed clear under an auto band"}
	}
	// A POLL_PAUSE band is HUMAN-OWNED: the reconciler NEVER auto-closes it to Done — not even on a confirmed
	// clear — because that would record a silent "human_resolved" close with no human in the loop. It goes to
	// To Verify (left open for the human), the predecessor's "non-AUTO band ⇒ To Verify, never Done" rule.
	return ReconcileDecision{Close: false, Ticket: TicketToVerify, Resolution: ResEscalated, Reason: "poll-band incident is human-owned — not auto-closed by the reconciler"}
}
