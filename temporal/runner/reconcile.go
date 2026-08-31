package runner

import (
	"context"
	"log"
	"time"

	tracker "github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/lessons"
	"github.com/territory-grounder/grounder/core/reconcile"
	"github.com/territory-grounder/grounder/core/safety"
)

// GraduationActivity applies ONLY the ladder credit for a finished session, and is dispatched by the workflow
// with MaximumAttempts:1.
//
// It is a separate activity for one reason: Temporal activities are AT-LEAST-ONCE, and the ladder write is not
// idempotent (Ladder.Record does a bare CleanRunCount++ with no dedupe key). Sharing ReconcileActivity's
// 4-attempt retry meant one confirmed heal could be credited up to four times — four of the five runs an
// op-class needs to reach AUTO, from a single incident. The rest of the reconcile work (tracker close-out,
// novelty writeback, resolved-incident lesson) is genuinely retry-safe and keeps its retries; only this write
// is pinned. An error here loses ONE credit, which is the correct failure direction for an earn path.
func (a *Activities) GraduationActivity(ctx context.Context, in ReconcileInput) error {
	a.recordGraduationCredit(ctx, in, reconcile.FinishedSession{
		ExternalRef:       in.ExternalRef,
		SessionID:         in.SessionID,
		ActionID:          in.ActionID,
		Band:              in.Band,
		HasVerdict:        in.HasVerdict,
		Verdict:           in.Verdict,
		ConfirmedClear:    in.ConfirmedClear,
		HasTerminalResult: in.HasTerminalResult,
		PollUnanswered:     in.PollUnanswered,
		ReCheckAttempts:    in.ReCheckAttempts,
		Executed:           in.Executed,
		CollateralObserved: in.CollateralObserved,
	})
	return nil
}

// recordGraduationCredit applies the terminus's clean-run decision to the ladder. Extracted so it can be
// invoked EITHER inline (legacy workflow versions, which have no separate activity in their history) or from
// GraduationActivity, which the current workflow version dispatches with MaximumAttempts:1.
func (a *Activities) recordGraduationCredit(ctx context.Context, in ReconcileInput, s reconcile.FinishedSession) {
	if a.D.RecordGraduation == nil || in.OpClass == "" || !s.Executed || !s.HasTerminalResult {
		return
	}
	// REQ-2907 (spec/029 T-029-4): a COMMIT-CONFIRMED run's ladder outcome belongs to its armed
	// window, not the session terminus — the window is still ARMED here (600s > any session), and
	// armed never counts (the spec/017 pending-never-counts discipline). The window's resolution
	// feeds the ladder from ResolveCommitConfirmActivity: confirmed → the one promoting outcome;
	// fired/failed revert → demotion; aborted/held → nothing. WITHHOLDING here (rather than
	// crediting now and correcting later) is what makes a clean-looking execution that fails its
	// confirm window unable to promote first — the exact laundering REQ-2907 exists to refuse.
	// Eligibility is registry data read in this activity (recorded result, replay-safe).
	if spec, ok := opschema.Lookup(in.OpClass); ok && spec.CommitConfirmed != nil {
		return
	}
	// TG-483: a terminus-observed collateral surfacing DEMOTES — the frozen execute-time MATCH plus a
	// cleared incident host is no longer sufficient when a sibling in the blast radius lit up meanwhile.
	clean := !s.PollUnanswered && s.HasVerdict && s.Verdict == safety.VerdictMatch && s.ConfirmedClear && !s.CollateralObserved
	// The session ref is the exactly-once credit key (TG-266/REQ-2804): this function is invoked from a
	// retried activity AND inline on legacy workflow versions, so the same terminus can reach the ladder
	// more than once for one incident.
	if err := a.D.RecordGraduation(ctx, in.OpClass, s.ExternalRef, clean); err != nil {
		log.Printf("reconcile graduation %s (%s clean=%v): %v (best-effort — the session terminus is unaffected)",
			s.ExternalRef, in.OpClass, clean, err)
	}
}

// ReconcileInput carries the terminal session facts the band-aware reconciler (spec/003) decides on. It
// is captured DATA — every field is set deterministically by the workflow from server-side state, never
// from agent free-text (INV-11): ConfirmedClear is an orchestrator-captured post-condition confirmation,
// never the acting agent's asserted success.
type ReconcileInput struct {
	ExternalRef       string
	SessionID         string
	ActionID          string
	Band              safety.Band
	HasVerdict        bool
	Verdict           safety.Verdict
	ConfirmedClear    bool
	HasTerminalResult bool
	PollUnanswered    bool
	ReCheckAttempts   int
	Executed          bool
	// CollateralObserved is the TG-483 terminus re-check's POSITIVE reading: a (host, rule) first surfaced
	// inside the action target's blast radius during the settle window, per TG's own durable capture. It
	// blocks auto-close (reconcile.Reconcile) and graduation credit (recordGraduationCredit) — the frozen
	// execute-time MATCH no longer outranks damage that surfaced later. Payload-only addition: same
	// activities, same sequence; legacy replays carry false and behave exactly as before.
	CollateralObserved bool
	// OpClass is the op-class whose graduation ladder this terminus feeds (REQ-1223). Payload-only addition:
	// same activity, same sequence, so no workflow version guard is required. Empty ⇒ no ladder write.
	OpClass string
	// SkipGraduation tells ReconcileActivity that the ladder write is being dispatched separately, as its own
	// MaximumAttempts:1 activity. Payload-only, and FALSE for every replaying legacy execution — so an
	// in-flight workflow started before the version guard keeps the old inline behaviour and neither
	// double-writes nor silently loses its credit.
	SkipGraduation bool
	// --- novelty writeback signature (TG-124) ---
	// Host and AlertRule are the STABLE incident signature the classifier's novelty gate keys on: Host is the
	// incident SUBJECT (env.Host — the ingest-validated alerted device, which ClassifyInput.IncidentHost
	// carries into the novelty read and which the clear confirmation was observed against), NOT the LLM-
	// expressed action target, and AlertRule is env.AlertRule. On a confirmed-clean terminus the resolved-
	// incident lesson is keyed on this same shape, so knowledge.Count sees a matching precedent and the next
	// same-subject incident is no longer flagged NOVEL — regardless of how the next proposal expresses its
	// action target. Site/Summary enrich the distilled precedent's retrieval; Action is the op that resolved
	// it (it becomes the lesson's Resolution). Populated only on the executed/proposed terminus that actually
	// ran an action; the stood-down/orphaned-poll terminus leaves them empty.
	Host      string
	AlertRule string
	Site      string
	Summary   string
	Action    string
}

// ReconcileResult is the serializable outcome of the terminal reconcile.
type ReconcileResult struct {
	ClosedOut        bool   // a close-out record was written (the ticket transitioned + the ledger appended)
	Closed           bool   // the incident was CLOSED (Done); false ⇒ left OPEN (To Verify)
	Ticket           string // the tracker state the incident transitioned to
	Resolution       string // the recorded resolution_type
	Reason           string // the reconciler's rationale (audited)
	ReCheckHandedOff bool   // an unresolved decision was requeued into the escalation re-check lane
}

// ReconcileActivity is the TERMINAL close-out lane (spec/003, REQ-201..208) wired into the Runner: at a
// genuinely-finished session it drives reconcile.Reconcile → reconcile.CloseOut to transition the
// incident's TRACKER ticket (a tracker write — annotate/transition — never an estate mutation) and append
// the close-out decision to the hash-chained governance ledger (INV-19), and it hands an UNRESOLVED
// decision (an orphaned poll the reconciler flags for a re-check) off to the escalation requeue lane.
//
// FAIL-SAFE by construction and mutation-OFF: it NEVER returns an activity error (a close-out/hand-off
// failure must never fail a session terminus), it NEVER actuates the estate (tracker + ledger +
// escalation-queue writes only), and every collaborator is OPTIONAL — a nil ledger/tracker skips the
// close-out, a nil re-check seam skips the hand-off. deviation→never-auto is preserved entirely by
// reconcile.Reconcile: a non-match verdict (a deviation/partial) is routed To Verify, never auto-closed to
// Done.
func (a *Activities) ReconcileActivity(ctx context.Context, in ReconcileInput) (ReconcileResult, error) {
	s := reconcile.FinishedSession{
		ExternalRef:       in.ExternalRef,
		SessionID:         in.SessionID,
		ActionID:          in.ActionID,
		Band:              in.Band,
		HasVerdict:        in.HasVerdict,
		Verdict:           in.Verdict,
		ConfirmedClear:    in.ConfirmedClear,
		HasTerminalResult: in.HasTerminalResult,
		PollUnanswered:     in.PollUnanswered,
		ReCheckAttempts:    in.ReCheckAttempts,
		Executed:           in.Executed,
		CollateralObserved: in.CollateralObserved,
	}
	d := reconcile.Reconcile(s)
	res := ReconcileResult{Closed: d.Close, Ticket: string(d.Ticket), Resolution: string(d.Resolution), Reason: d.Reason}

	// Close-out: transition the tracker ticket and append the close-out governance decision. Requires BOTH a
	// ledger and a tracker seam; either missing ⇒ the decision still stands (returned) but no close-out is
	// written. A CloseOut error (a tracker/ledger outage) is SWALLOWED + logged — the session terminus is
	// never blocked by a close-out failure.
	if a.D.Ledger != nil && a.D.Tickets != nil {
		if rec, err := reconcile.CloseOut(ctx, a.D.Ledger, a.D.Tickets, s, d); err != nil {
			log.Printf("reconcile close-out %s: %v (best-effort — the session terminus is unaffected)", in.ExternalRef, err)
		} else {
			res.ClosedOut = true
			res.Closed = rec.Closed
		}
	}

	// Novelty WRITEBACK (TG-124): the learn hop of observe→resolve→learn→retrieve. A CONFIRMED-CLEAN terminus —
	// a clean MATCH verdict AND an orchestrator-confirmed clear on a genuinely-terminal, non-orphaned session
	// (mirroring the lessons.Lesson distill gate EXACTLY) — is distilled into a citable precedent keyed on the
	// SAME (host, alert_rule) signature the classifier's novelty gate read (in.Host = the incident subject
	// env.Host, in.AlertRule = env.AlertRule). This de-novels the (host, rule): the FIRST occurrence is novel → POLL_PAUSE
	// → a human approves and the fix verifies clean → THIS writeback records the precedent → the next same-shape
	// incident is no longer novel, so a graduated op-class stops POLL_PAUSE-ing it forever. It is deliberately
	// BAND-INDEPENDENT: the first-occurrence de-novel is precisely a POLL_PAUSE-band resolution, which the
	// reconciler routes To Verify (d.Close=false) — so gating on the auto-close DECISION would defeat the fix;
	// the gate is on the confirmed-clean session FACTS instead. It emits NOTHING on a
	// deviation/partial/unverified/orphaned-poll/crash outcome (the gate fails closed — when in doubt do not
	// write; a false precedent would silence novelty for an incident TG did not actually resolve). The
	// persistence seam re-applies the same lessons gate (defense in depth; a ref-less/action-less record is
	// dropped there). Best-effort + mutation OFF: a nil seam or a write error never fails the session terminus,
	// and it writes ONLY the knowledge corpus (a file/in-memory reload) — never the estate.
	// GRADUATION IS DECIDED HERE, NOT AT EXECUTE (REQ-1223). The interceptor's post-execution verdict is taken
	// ~1s after the effect, against a monitoring surface whose own poll cycle is minutes long, with a baseline
	// that subtracts everything already firing — so `match` is very nearly structural and cannot, on its own,
	// distinguish a heal that WORKED from one whose consequences had not surfaced yet. Promoting on it let an
	// op-class climb toward AUTO on that non-signal. This terminus is the earliest point that carries a
	// re-observation from AFTER the monitoring surface refreshed: the SAME confirmed-clean facts the novelty
	// writeback below already trusts to de-novel an incident. If they are good enough to remove a human's
	// first-sight review, they are the right evidence for autonomy.
	//
	// Only the CLEAN RUN is decided here. A deviation still demotes IMMEDIATELY at the interceptor — a fast
	// safety response must not wait for a settle window. So the immediate path may demote or break a streak but
	// can never promote, and this path is the sole promoter.
	//
	// AT-LEAST-ONCE IS THE HAZARD, and this write used to sit in the wrong activity for it. ReconcileActivity
	// runs on runnerActivityOptions() — StartToCloseTimeout 1m, MaximumAttempts 4 — while Ladder.Record does a
	// bare CleanRunCount++ with no dedupe key. A single confirmed heal that timed out mid-activity (the
	// YouTrack CloseOut on the same path has no client timeout) could therefore be retried and credited up to
	// FOUR times, delivering four of the five runs the ladder demands from ONE incident. The execute activity
	// two files over is pinned to MaximumAttempts:1 for exactly this reason, in a comment that states the rule
	// as law; the earn path deserves the same discipline. SkipGraduation is set by the versioned workflow path
	// that now dispatches this write as its OWN single-attempt activity — losing a credit on failure, which is
	// the correct direction for an autonomy earn path.
	if !in.SkipGraduation {
		a.recordGraduationCredit(ctx, in, s)
	}
	if a.D.LearnResolved != nil && s.HasTerminalResult && !s.PollUnanswered &&
		s.Executed && s.HasVerdict && s.Verdict == safety.VerdictMatch && s.ConfirmedClear {
		if err := a.D.LearnResolved(ctx, lessons.ResolvedIncident{
			ExternalRef:    s.ExternalRef,
			Host:           in.Host,      // the incident SUBJECT (env.Host) — the stable novelty key knowledge.Count reads
			AlertRule:      in.AlertRule, // env.AlertRule — the same rule the classifier keyed novelty on
			Site:           in.Site,
			Summary:        in.Summary,
			Action:         in.Action,
			Verdict:        s.Verdict,
			ConfirmedClear: s.ConfirmedClear,
			ResolvedAt:     time.Now().UTC(),
		}); err != nil {
			log.Printf("reconcile novelty-writeback %s: %v (best-effort — the session terminus is unaffected)", s.ExternalRef, err)
		}
	} else if a.D.LearnResolved != nil && s.Executed && s.HasTerminalResult {
		// Writeback-gate DIAGNOSTICS (TG-124): an EXECUTED, terminal session that did NOT reach the writeback is
		// the observed silent-miss shape (openwebui01/actualbudget01 healed but never de-noveled). The gate reads
		// in-memory facts that are otherwise unlogged, so name exactly which precondition withheld the precedent —
		// turning a silent miss into a diagnosable event. Observation-only (a log line), never affects the terminus.
		log.Printf("reconcile writeback WITHHELD %s (host=%s rule=%s action=%q): hasVerdict=%v verdict=%q verdictMatch=%v confirmedClear=%v pollAnswered=%v",
			s.ExternalRef, in.Host, in.AlertRule, in.Action, s.HasVerdict, s.Verdict, s.Verdict == safety.VerdictMatch, s.ConfirmedClear, !s.PollUnanswered)
	}

	// The reconcile → escalation re-check hand-off (spec/003 REQ-206): an orphaned poll the reconciler flags
	// for a re-check is requeued into the escalation lane, so an unresolved incident is re-examined against
	// the LIVE condition and converges to a human — never silently dropped. Rate-capped by the escalation
	// controller's per-incident cap (it stands down to a human at the cap). Fail-safe: a nil seam ⇒ no
	// hand-off; a hand-off error is SWALLOWED + logged. It writes ONLY the escalation queue — never the estate.
	if d.ScheduleReCheck && a.D.ReCheckSchedule != nil {
		if err := a.D.ReCheckSchedule(ctx, in.ExternalRef, d.ReCheckAttempts); err != nil {
			log.Printf("reconcile re-check hand-off %s: %v (best-effort — the terminus is unaffected)", in.ExternalRef, err)
		} else {
			res.ReCheckHandedOff = true
		}
	}
	return res, nil
}

// trackerTransitioner adapts a tracker.Tracker to the reconcile.TicketTransitioner seam: a close-out
// transitions the incident's ticket through the tracker module — a TRACKER write (annotate/transition),
// never an estate mutation and never gated by the mutation chokepoint. "To Verify" (left open, not a
// silent close) maps to in_progress; "Done" to resolved.
type trackerTransitioner struct{ t tracker.Tracker }

// NewTrackerTransitioner wraps a tracker as the terminal reconcile's ticket seam (runner.Deps.Tickets). A
// nil tracker yields nil, so the reconcile records no close-out (fail-safe — the composition root wires it
// only when exactly one tracker is enabled).
func NewTrackerTransitioner(t tracker.Tracker) reconcile.TicketTransitioner {
	if t == nil {
		return nil
	}
	return trackerTransitioner{t: t}
}

func (a trackerTransitioner) Transition(ctx context.Context, externalRef string, to reconcile.TicketState) error {
	st := tracker.StateInProgress // "To Verify" — the incident stays OPEN, not silently closed
	if to == reconcile.TicketDone {
		st = tracker.StateResolved
	}
	return a.t.TransitionState(ctx, externalRef, st)
}
