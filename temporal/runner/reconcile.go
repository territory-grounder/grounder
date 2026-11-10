package runner

import (
	"context"
	"log"
	"time"

	tracker "github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/db"
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
		ExternalRef:        in.ExternalRef,
		SessionID:          in.SessionID,
		ActionID:           in.ActionID,
		Band:               in.Band,
		HasVerdict:         in.HasVerdict,
		Verdict:            in.Verdict,
		ConfirmedClear:     in.ConfirmedClear,
		HasTerminalResult:  in.HasTerminalResult,
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
	if in.OpClass == "" || !s.Executed || !s.HasTerminalResult {
		return
	}
	d := a.creditDecision(ctx, in, s)
	a.appendTerminusRecord(ctx, in, s, d)
	if !d.feed {
		log.Printf("reconcile graduation %s (%s): %s — credit WITHHELD, the ladder is not fed", s.ExternalRef, in.OpClass, d.disposition)
		return
	}
	if a.D.RecordGraduation == nil {
		return
	}
	// The session ref is the exactly-once credit key (TG-266/REQ-2804): this function is invoked from a
	// retried activity AND inline on legacy workflow versions, so the same terminus can reach the ladder
	// more than once for one incident.
	if err := a.D.RecordGraduation(ctx, in.OpClass, s.ExternalRef, d.clean); err != nil {
		log.Printf("reconcile graduation %s (%s clean=%v %s): %v (best-effort — the session terminus is unaffected)",
			s.ExternalRef, in.OpClass, d.clean, d.disposition, err)
	}
}

// creditDecision is the terminus's EXPLICIT classification of one executed session (owner prompt 2026-11-07 §2.D):
// `clean` (fed as a verified clean run), `withheld:<reason>` (nothing fed — the run was not shown bad, but it was
// not shown TG's clean heal either) or `not-clean:<reason>` (fed as a streak break). The reason names the FIRST
// failing conjunct so the record says exactly why.
type creditDecision struct {
	targetVerified bool
	clean          bool
	feed           bool
	disposition    string
	windowState    string
}

// targetVerified is the target-state half of the credit predicate: the poll was answered ∧ a verdict exists ∧ it
// read match ∧ the incident CONFIRMED CLEAR (host quiet AND the healed object positively in its goal state —
// workflow.go's clear loop) ∧ the TG-483 collateral re-check OBSERVED a quiet blast radius. Shared by the credit
// and the precedent lesson so the two can never disagree about a session.
func targetVerified(in ReconcileInput, s reconcile.FinishedSession) (bool, string) {
	switch {
	case s.PollUnanswered:
		return false, "poll-unanswered"
	case !s.HasVerdict:
		return false, "no-verdict"
	case s.Verdict != safety.VerdictMatch:
		return false, "verdict-" + string(s.Verdict)
	case !s.ConfirmedClear:
		if in.ClearReason != "" {
			return false, "not-confirmed-clear:" + in.ClearReason
		}
		return false, "not-confirmed-clear"
	case !in.CollateralKnown:
		return false, "collateral-unknown"
	case s.CollateralObserved:
		return false, "collateral-observed"
	}
	return true, ""
}

// THE CREDIT PREDICATE (TG-662, G4A F3/F8; supersedes the T-029-4 terminus withhold). The terminus is the ONE
// author of the clean-run decision for EVERY class. A verified `match` is NECESSARY, never sufficient: it
// EXCLUDES the action's own target host (core/verify/verdict.go), so on its own it cannot say the heal worked.
//
//   - a target state NOT verified (see targetVerified) feeds clean=false: a streak break is a safety signal no
//     bookkeeping may withhold. An UNKNOWN collateral reading or an unobserved object is INDETERMINATE (TG-668):
//     it never credits, and like every other non-clean terminus it breaks the streak.
//   - a verified target state whose CAUSALITY is not `tg-caused` (an injector restore, another actor, a
//     recovery that preceded the execution, or unreadable evidence) is WITHHELD: nothing is fed. The heal was
//     not shown bad, but TG was not shown to have caused the recovery — indeterminate never collapses into
//     verified (owner prompt 2026-11-07 §2.D).
//   - a COMMIT-CONFIRMED class (REQ-2907) is credited only once its window has RESOLVED `confirmed`. The
//     window's consult reads the SAME persisted facts, so the two authors cannot disagree — they can only race:
//     when the window has not confirmed yet (the post-facts nudge is in flight) the terminus WITHHOLDS and the
//     window's own confirmed feed (ResolveCommitConfirmActivity) credits it; the graduation_credit claim key
//     dedups whichever lands second. Eligibility is registry data read in this activity (recorded, replay-safe).
func (a *Activities) creditDecision(ctx context.Context, in ReconcileInput, s reconcile.FinishedSession) creditDecision {
	d := creditDecision{}
	ok, reason := targetVerified(in, s)
	if !ok {
		d.feed, d.disposition = true, "not-clean:"+reason
		return d
	}
	d.targetVerified = true
	if in.Causality != CausalityTGCaused {
		c := in.Causality
		if c == "" {
			c = CausalityIndeterminate
		}
		d.disposition = "withheld:causality-" + c
		return d
	}
	if spec, ok := opschema.Lookup(in.OpClass); ok && spec.CommitConfirmed != nil {
		row, err := a.commitConfirmRowFor(ctx, in.ActionID, s.ExternalRef)
		state := "unreadable"
		if err == nil {
			state = row.State
		}
		d.windowState = state
		if err != nil || row.State != db.CommitConfirmConfirmed {
			d.disposition = "withheld:window-" + state
			return d
		}
	}
	d.clean, d.feed, d.disposition = true, true, "clean"
	return d
}

// TerminusRecord is the runner-side row the composition root appends to terminus_record (append-only, migration
// 0122): everything the credit predicate saw and decided for one executed session, plus the causality.
type TerminusRecord struct {
	ExternalRef       string
	ActionID          string
	OpClass           string
	Host              string
	TargetHost        string
	Site              string
	ExecutedAt        time.Time
	ClearedAt         time.Time
	Executed          bool
	HasVerdict        bool
	Verdict           string
	ConfirmedClear    bool
	ClearReason       string
	ObjectState       string
	Collateral        string
	CreditDisposition string
	CreditFed         bool
	CreditClean       bool
	Causality         string
	CausalityEvidence string
	WindowState       string
	// Source: "" / "terminus" for the session terminus's own row, "window" for the commit-confirm window's
	// resolution feed (ResolveCommitConfirmActivity) — see migration 0122's source column.
	Source string
}

// appendTerminusRecord writes the record through the seam (best-effort, observe-only — a lost row never changes
// the terminus; the ledger and the log still carry the decision).
func (a *Activities) appendTerminusRecord(ctx context.Context, in ReconcileInput, s reconcile.FinishedSession, d creditDecision) {
	if a.D.TerminusRecord == nil {
		return
	}
	collateral := "unknown"
	if in.CollateralKnown {
		collateral = "quiet"
		if s.CollateralObserved {
			collateral = "positive"
		}
	}
	causality := in.Causality
	if causality == "" {
		causality = CausalityIndeterminate
	}
	rec := TerminusRecord{
		ExternalRef: s.ExternalRef, ActionID: in.ActionID, OpClass: in.OpClass, Host: in.Host, TargetHost: in.TargetHost, Site: in.Site,
		ExecutedAt: in.ExecutedAt, ClearedAt: in.ClearedAt, Executed: s.Executed, HasVerdict: s.HasVerdict, Verdict: string(s.Verdict),
		ConfirmedClear: s.ConfirmedClear, ClearReason: in.ClearReason, ObjectState: in.ObjectState, Collateral: collateral,
		CreditDisposition: d.disposition, CreditFed: d.feed, CreditClean: d.clean,
		Causality: causality, CausalityEvidence: in.CausalityEvidence, WindowState: d.windowState,
	}
	if err := a.D.TerminusRecord(ctx, rec); err != nil {
		log.Printf("reconcile terminus-record %s (%s): %v (best-effort — the decision stands in the log and the ledger)", s.ExternalRef, in.OpClass, err)
	}
}

// ReconcileInput carries the terminal session facts the band-aware reconciler (spec/003) decides on. It
// is captured DATA — every field is set deterministically by the workflow from server-side state, never
// from agent free-text (INV-11): ConfirmedClear is an orchestrator-captured post-condition confirmation,
// never the acting agent's asserted success.
type ReconcileInput struct {
	// TG-662 terminus facts for the record (owner prompt 2026-11-07 §2.D): the action's target, when it executed
	// and when the clear confirmed (workflow clock), why the clear did NOT confirm (host-alerting | object-unhealthy
	// | object-unobserved | …), the last object reading, and the causality the facts activity classified with its
	// evidence. Payload-only — captured by the workflow, never read live here.
	TargetHost        string
	ExecutedAt        time.Time
	ClearedAt         time.Time
	ClearReason       string
	ObjectState       string
	Causality         string
	CausalityEvidence string
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
	// CollateralKnown reports whether the re-check OBSERVED at all (TG-662/TG-668): true with Observed false is
	// "surveyed and quiet", the only reading that credits; false is UNKNOWN (no graph members, no reader, an
	// activity error) and is indeterminate — never a clean run. Payload-only addition: legacy replays carry
	// false and therefore never credit a run whose collateral was never surveyed.
	CollateralKnown bool
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
	// Prediction is the committed machine prediction rendered judge-readable (RunnerResult.Prediction) — the
	// experience reflector (TG-579) reasons about the verifier's verdict AGAINST it, so the prompt must see
	// the real text or say it has none. Payload-only addition; legacy replays carry "" and the reflector's
	// mission says so instead of inviting the model to infer one.
	Prediction string
}

// ReflectRequest is what the experience reflector seam (Deps.Reflect, TG-579) receives: the resolved
// incident the caution lane also sees, plus the COMMITTED PREDICTION the verifier judged — the one input the
// reflector's prompt reasons about that lessons.ResolvedIncident does not carry. Prediction is the
// judge-readable rendering (RunnerResult.Prediction), empty when nothing was predicted.
type ReflectRequest struct {
	Incident   lessons.ResolvedIncident
	Prediction string
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
		ExternalRef:        in.ExternalRef,
		SessionID:          in.SessionID,
		ActionID:           in.ActionID,
		Band:               in.Band,
		HasVerdict:         in.HasVerdict,
		Verdict:            in.Verdict,
		ConfirmedClear:     in.ConfirmedClear,
		HasTerminalResult:  in.HasTerminalResult,
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
	// A precedent needs a CLEAN run: match, confirmed clear, AND no collateral (TG-673, deep-review F14). A heal
	// whose blast radius surfaced a new (host, rule) during the settle window already loses auto-close and the
	// graduation credit (TG-483); until this line it still became a "verified-resolution" lesson and de-noveled
	// the incident, so TG's collateral damage was taught back to it as what worked. TG-662 tightens it to the
	// SAME predicate the credit uses (targetVerified: collateral must be KNOWN quiet, not merely not-observed) AND
	// to causality tg-caused: a lesson "this action resolved this incident" when the injector or another actor
	// restored it is exactly the contaminated learning the owner's 2026-11-07 prompt forbids (§2.D "no clean
	// lesson for unresolved or indeterminate outcomes").
	lessonVerified, lessonReason := targetVerified(in, s)
	lessonVerified = lessonVerified && s.Executed && s.HasTerminalResult
	tgCaused := in.Causality == CausalityTGCaused
	if a.D.LearnResolved != nil && lessonVerified && tgCaused {
		if err := a.D.LearnResolved(ctx, lessons.ResolvedIncident{
			ExternalRef:    s.ExternalRef,
			Host:           in.Host,      // the incident SUBJECT (env.Host) — the stable novelty key knowledge.Count reads
			AlertRule:      in.AlertRule, // env.AlertRule — the same rule the classifier keyed novelty on
			Site:           in.Site,
			Summary:        in.Summary,
			Action:         in.Action,
			Verdict:        s.Verdict,
			ConfirmedClear: s.ConfirmedClear,
			Collateral:     s.CollateralObserved,
			ResolvedAt:     time.Now().UTC(),
		}); err != nil {
			log.Printf("reconcile novelty-writeback %s: %v (best-effort — the session terminus is unaffected)", s.ExternalRef, err)
		}
	} else if a.D.LearnResolved != nil && s.Executed && s.HasTerminalResult {
		// Writeback-gate DIAGNOSTICS (TG-124): an EXECUTED, terminal session that did NOT reach the writeback is
		// the observed silent-miss shape (openwebui01/actualbudget01 healed but never de-noveled). The gate reads
		// in-memory facts that are otherwise unlogged, so name exactly which precondition withheld the precedent —
		// turning a silent miss into a diagnosable event. Observation-only (a log line), never affects the terminus.
		log.Printf("reconcile writeback WITHHELD %s (host=%s rule=%s action=%q): targetVerified=%v reason=%q causality=%q hasVerdict=%v verdict=%q confirmedClear=%v collateralKnown=%v collateral=%v pollAnswered=%v",
			s.ExternalRef, in.Host, in.AlertRule, in.Action, lessonVerified, lessonReason, in.Causality, s.HasVerdict, s.Verdict, s.ConfirmedClear, in.CollateralKnown, s.CollateralObserved, !s.PollUnanswered)
	}
	// THE COMPLEMENT (TG-52 live writer + TG-579 reflector). Every executed, terminal, poll-answered session
	// whose TARGET STATE was not verified is the caution lane's subject — a deviation, a partial, an unconfirmed
	// match, an unknown collateral. A verified target state with an unestablished causality is NEITHER: the heal
	// was not shown bad, so it is no caution, and not shown TG's, so it is no precedent (logged WITHHELD above).
	// The two writers below are best-effort and observe-only (a write error never fails the terminus), and both
	// write ONLY their own corpus/table — never the estate, never anything a gate reads. The reflector is bounded
	// by its own timeout inside the seam so a slow model cannot hold the reconcile activity past its deadline.
	if s.Executed && s.HasTerminalResult && !s.PollUnanswered && !lessonVerified &&
		(a.D.LearnCaution != nil || a.D.Reflect != nil) {
		ri := lessons.ResolvedIncident{
			ExternalRef: s.ExternalRef, Host: in.Host, AlertRule: in.AlertRule, Site: in.Site,
			Summary: in.Summary, Action: in.Action, Verdict: s.Verdict, ConfirmedClear: s.ConfirmedClear,
			Collateral: s.CollateralObserved, ResolvedAt: time.Now().UTC(),
		}
		if a.D.LearnCaution != nil {
			if err := a.D.LearnCaution(ctx, ri); err != nil {
				log.Printf("reconcile caution-writeback %s: %v (best-effort — the session terminus is unaffected)", s.ExternalRef, err)
			}
		}
		if a.D.Reflect != nil {
			if err := a.D.Reflect(ctx, ReflectRequest{Incident: ri, Prediction: in.Prediction}); err != nil {
				log.Printf("reconcile experience-reflector %s: %v (best-effort — the session terminus is unaffected)", s.ExternalRef, err)
			}
		}
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
