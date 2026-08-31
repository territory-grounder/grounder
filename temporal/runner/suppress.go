package runner

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	coregov "github.com/territory-grounder/grounder/core/governance"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/observe"
	"github.com/territory-grounder/grounder/core/suppression"
)

// suppressor decides whether an incident is suppressed before a session is spent (spec/005). A
// *suppression.Chain satisfies it directly (static stages only); the LiveSuppressGate below wraps a chain
// with a LIVE recent-triage log so the dedup stage sees the incidents this worker recently triaged. Deps
// carries the interface so the oracle can inject a plain chain and production the live gate.
type suppressor interface {
	Decide(ctx context.Context, a suppression.Alert, now time.Time) (suppression.Decision, error)
}

// RecentTriageLog is the worker's in-memory, concurrency-safe, time-windowed memory of recently triaged
// (host, alert_rule) incidents — the anchor set the dedup stage scans so a re-fire of an OPEN incident within
// the window does not spawn a second session. It is best-effort by design: entries live at most `retention`
// and are evicted lazily on read, and the log is per-worker (a restart or a second worker simply forgets some
// recent triages). Dedup is fail-open — forgetting an anchor costs at most one extra session, never a missed
// real incident — so an in-memory single-worker log is a sound default; a durable shared log can replace it
// behind the same seam without touching the gate.
type RecentTriageLog struct {
	mu        sync.Mutex
	entries   []suppression.TriageEntry
	retention time.Duration
}

// NewRecentTriageLog returns a log that retains entries for the given window.
func NewRecentTriageLog(retention time.Duration) *RecentTriageLog {
	return &RecentTriageLog{retention: retention}
}

// Record appends a triage entry (best-effort; a retried activity may double-record, which dedup tolerates).
func (l *RecentTriageLog) Record(e suppression.TriageEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
}

// Recent returns the entries within `window` of now and, in the same pass, evicts everything older than the
// log's retention — so the slice cannot grow without bound. A copy is returned; callers never alias the log.
func (l *RecentTriageLog) Recent(now time.Time, window time.Duration) []suppression.TriageEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.entries[:0]
	var out []suppression.TriageEntry
	for _, e := range l.entries {
		age := now.Sub(e.LoggedAt)
		if age < 0 || age > l.retention {
			continue // future-dated or past retention — drop
		}
		kept = append(kept, e)
		if age <= window {
			out = append(out, e)
		}
	}
	// zero the tail so evicted entries can be GC'd, then truncate.
	for i := len(kept); i < len(l.entries); i++ {
		l.entries[i] = suppression.TriageEntry{}
	}
	l.entries = kept
	return out
}

// LiveSuppressGate is the production suppressor: it assembles a suppression.Chain PER INCIDENT from the
// static operator-curated config (freeze windows, active-memory rules) PLUS a live dedup stage backed by the
// recent-triage log, runs it, and records the triage back into the log. Assembling per incident is what makes
// the dedup stage see live state (the DedupStage's anchor set is a fixed slice, so it must be re-supplied each
// time). Stage order matches spec/005: freeze (in Chain) → severity floor → dedup → operator rule.
type LiveSuppressGate struct {
	Freeze          *suppression.FreezeGate
	Folds           []suppression.SuppressionPolicy
	FoldFreshness   time.Duration
	Schedules       []suppression.Schedule
	RebootPreBuffer time.Duration // how long BEFORE a scheduled fire a reboot alert still matches (default 5m)
	RebootWindow    time.Duration // how long AFTER  a scheduled fire a reboot alert still matches (default 10m)
	Patterns        []suppression.TransientPattern
	Rules           []suppression.SuppressRule
	Window          time.Duration
	OpenIssue       func(issueRef string) bool
	Ledger          *audit.Ledger
	Log             *RecentTriageLog
	// Clock is the DEDUP evaluation clock: the wall-clock instant this worker is triaging, used to stamp the
	// recent-triage log and to measure the dedup window — deliberately distinct from the alert's observation
	// time (the `now` passed to Decide, which freeze/scheduled match on). Nil ⇒ time.Now(), which is the
	// correct production value; it exists only so an oracle can pin triage time deterministically. See TG-377:
	// feeding the alert clock into the dedup window let a storm of out-of-order arrivals suppress 0 of 171.
	Clock func() time.Time

	// ---- the LEARNED scheduled-reboot lane (spec/005 REQ-409..411, TG-219) ----
	// Learn is the observe→verify→promote writer. Nil ⇒ the lane is DARK: only operator-declared schedules
	// are honored and nothing is learned (the default; armed by TG_SUPPRESSION_LEARN_ENABLED).
	Learn *suppression.Learner
	// Demotions is the governance analysis-only state, consulted before a LEARNED schedule may suppress.
	Demotions suppression.DemotionLookup
	// Evidence records PROOF that a learned suppression silenced an incident that needed action, for the
	// scheduled demote pass to act on.
	Evidence coregov.EvidenceStore
	// LearnRenewFor is how far a matched learned schedule's validity is pushed out (renew-on-match).
	LearnRenewFor time.Duration

	// Stages is the TG-380 decision-stage instrument. nil is a no-op (the tally handles nil), so a gate
	// built without it is silently observation-free rather than a crash. Records the suppress stage's
	// offered/eligible/acted triple on every Decide.
	Stages *observe.StageTally

	countMu sync.Mutex
	counts  map[string]int // decision outcome (escalate/suppressed/notice) → running count, for telemetry
}

// Counts returns a snapshot of the gate's decision counts by outcome (for observability). Concurrency-safe.
func (g *LiveSuppressGate) Counts() map[string]int {
	g.countMu.Lock()
	defer g.countMu.Unlock()
	out := make(map[string]int, len(g.counts))
	for k, v := range g.counts {
		out[k] = v
	}
	return out
}

func (g *LiveSuppressGate) record(outcome string) {
	g.countMu.Lock()
	defer g.countMu.Unlock()
	if g.counts == nil {
		g.counts = map[string]int{}
	}
	g.counts[outcome]++
}

// Decide runs the assembled chain and records the incident into the live log.
func (g *LiveSuppressGate) Decide(ctx context.Context, a suppression.Alert, now time.Time) (suppression.Decision, error) {
	// `now` is the alert's OBSERVATION time — the clock freeze/scheduled match on (when the alert fired).
	// The dedup lane runs on a separate EVALUATION clock (when this worker is triaging), which is also the
	// clock the recent-triage log is stamped and queried with. Conflating the two was TG-377: out-of-order /
	// ingestion-lagged storm alerts read each other's anchors as future-dated and dedup suppressed 0 of 171.
	evalNow := g.evalClock()
	// spec/005 stage order: dedup → known-pattern → active-memory (blast-radius and scheduled join with their
	// stateful backing). First non-escalate wins.
	stages := []suppression.Stage{
		&suppression.DedupStage{Recent: g.Log.Recent(evalNow, g.Window), Window: g.Window, EvalAt: evalNow, OpenIssue: g.OpenIssue},
	}
	if len(g.Folds) > 0 {
		stages = append(stages, &suppression.BlastRadiusStage{Policies: g.Folds, Freshness: g.FoldFreshness})
	}
	// Phase SR matches BOTH lanes off ONE stage: the operator-declared rows (static config) plus the learned
	// rows that are LIVE right now. Reading the learned rows per incident is what makes the lane live — a
	// promotion or a demotion applies to the very next alert, not to the next worker restart.
	scheds := g.Schedules
	if learned := g.Learn.Live(); len(learned) > 0 {
		scheds = append(append(make([]suppression.Schedule, 0, len(g.Schedules)+len(learned)), g.Schedules...), learned...)
	}
	if len(scheds) > 0 {
		stages = append(stages, &suppression.ScheduledStage{
			Schedules: scheds,
			Window:    g.rebootWindow(),
			Demotions: g.Demotions,
			Renew:     g.renewer(), RenewFor: g.LearnRenewFor,
		})
	}
	if len(g.Patterns) > 0 {
		stages = append(stages, &suppression.KnownPatternStage{Patterns: g.Patterns})
	}
	if len(g.Rules) > 0 {
		stages = append(stages, &suppression.ActiveMemoryStage{Rules: g.Rules})
	}
	chain := &suppression.Chain{Freeze: g.Freeze, Stages: stages, Ledger: g.Ledger}
	d, err := chain.Decide(ctx, a, now)
	d = g.afterDecide(ctx, a, d, now)
	// Record this triage as a future dedup anchor. Suppressed is carried so a silenced alert is not itself a
	// valid anchor (you dedup a re-fire against a still-open INCIDENT, not against another suppressed alert),
	// and IssueRef carries the incident key so a re-fire is deduped only WHILE that incident is still open
	// (OpenIssue) — a re-fire after it resolved is a genuine new incident.
	g.Log.Record(suppression.TriageEntry{Host: a.Host, AlertRule: a.AlertRule, LoggedAt: evalNow, Suppressed: d.Outcome.Suppressing(), IssueRef: a.ExternalRef})
	g.record(d.Outcome.String())
	// TG-380 decision-stage triple. offered = every Decide. acted = suppressed/notice. eligible = the alert
	// was a genuine suppression candidate — it was NOT force-escalated by the severity floor (a critical or
	// unknown severity short-circuits before any stage runs, core/suppression Phase=severity + escalate).
	// So eligible = NOT (severity-phase escalate); a freeze/stage suppression is eligible-and-acted. This
	// makes a zero read cleanly: eligible=0 means "everything was critical/unknown, nothing to suppress",
	// distinct from eligible>0/acted=0 meaning "stages ran and suppressed nothing" (the dead-stage signal).
	// GRANULARITY NOTE (slice 1): this eligible bucket collapses two distinct not-acted causes — "ran every
	// stage, none matched" (Phase=escalate) and "a learned scheduled-reboot suppression was reversed
	// post-hoc" (afterDecide → Phase=scheduled-reboot, Outcome=escalate). Both are genuinely eligible (they
	// reached the stages); the resolution loss is deliberate for the triple and a finer breakdown is slice-2
	// work. The subset invariant holds in every Phase×Outcome the chain emits (Record also enforces it).
	eligible := !(d.Phase == suppression.PhaseSeverity && d.Outcome == suppression.OutcomeEscalate)
	g.Stages.Record("suppress", eligible, d.Outcome.Suppressing())
	return d, err
}

// evalClock is the dedup lane's triage clock: the injected Clock, or time.Now() in production. See the Clock
// field and TG-377 — the recent-triage log is stamped and the dedup window measured on THIS clock, never on
// the alert's observation time.
func (g *LiveSuppressGate) evalClock() time.Time {
	if g.Clock != nil {
		return g.Clock()
	}
	return time.Now()
}

func (g *LiveSuppressGate) rebootWindow() suppression.WindowEvaluator {
	return suppression.WindowEvaluator{PreBuffer: g.RebootPreBuffer, PostWindow: g.RebootWindow}
}

// renewer returns the registry to renew a matched schedule against, or nil when the learned lane is dark.
func (g *LiveSuppressGate) renewer() suppression.Renewer {
	if g.Learn == nil || g.Learn.Registry == nil {
		return nil
	}
	return g.Learn.Registry
}

// afterDecide is the learned lane's post-decision half, and it is where the lane earns the right to
// suppress at all. Two mutually exclusive branches:
//
//	VERIFY   — the chain suppressed on a LEARNED schedule. That suppression is PROVISIONAL: the two-phase
//	           verifier reads the recorded boot reason (REQ-406). A clean boot confirms it. A reactive or
//	           unknown boot means this lesson just darkened a crash — so the verifier reopens and pages, the
//	           row is demoted out of LIVE immediately, the miss is recorded as evidence for the scheduled
//	           demote pass, and THIS DECISION IS REVERSED to escalate. Reversing rather than merely reopening
//	           is deliberate: TG reads the boot reason off the incident itself, so the answer is already in
//	           hand when the decision is made, and a suppression known to be wrong must not be returned as a
//	           suppression.
//	LEARN    — the chain did NOT suppress a reboot-class alert. This is exactly where the predecessor's
//	           reactive arm runs (classify-reboot-alert.py at triage, "when a reboot-class alert was NOT
//	           suppressed by the matcher"): offer it to the learner, which applies the clean-boot gate and
//	           the observe→verify→promote lifecycle.
//
// Everything here is best-effort with respect to the CURRENT alert: a learner error never suppresses, and
// the only way this function changes a decision is toward escalation.
func (g *LiveSuppressGate) afterDecide(ctx context.Context, a suppression.Alert, d suppression.Decision, now time.Time) suppression.Decision {
	if g.Learn == nil {
		return d
	}
	if d.Outcome.Suppressing() && d.Phase == suppression.PhaseScheduledReboot && d.Signals["schedule_source"] == suppression.SourceLearned.String() {
		return g.verifyLearnedSuppression(ctx, a, d, now)
	}
	if d.Outcome == suppression.OutcomeEscalate && a.IsReboot {
		out := g.Learn.Observe(ctx, suppression.RebootObservation{
			Host: a.Host, ExternalRef: a.ExternalRef, AlertRule: a.AlertRule,
			BootReason: a.BootReason, At: a.ObservedAt,
		}, now)
		if out.Registered {
			log.Printf("suppression(learn): %s", out.Reason)
		}
	}
	return d
}

// verifyLearnedSuppression runs the two-phase verify over a learned suppression and reverses it when the
// boot was not clean.
func (g *LiveSuppressGate) verifyLearnedSuppression(ctx context.Context, a suppression.Alert, d suppression.Decision, now time.Time) suppression.Decision {
	v := g.Learn.Verifier
	if v == nil {
		return d
	}
	res, verr := v.Verify(ctx, a.ExternalRef, a.BootReason)
	if verr != nil {
		// The reopen/page channel failed. The suppression is unconfirmed and the channel that would tell a
		// human is broken, so the alert is escalated here rather than left silently suppressed.
		log.Printf("suppression(learn): two-phase verify of %s failed to reopen/page: %v — escalating", a.ExternalRef, verr)
		return escalateReversal(a, "two-phase verify could not confirm the suppressed reboot (reopen/page failed)")
	}
	if res.Confirmed {
		return d
	}
	key := suppression.ScheduleKey{Host: a.Host, Kind: d.Signals["kind"], Cron: d.Signals["cron"]}
	g.Learn.Demote(key)
	if g.Evidence != nil {
		if err := g.Evidence.Record(ctx, coregov.SuppressionEvidence{
			Tuple:       coregov.Tuple{Host: a.Host, AlertRule: a.AlertRule},
			ExternalRef: a.ExternalRef,
			Detail:      "learned schedule " + key.Cron + " suppressed a reboot whose boot reason was not clean: " + res.BootReason,
			ObservedAt:  now,
		}); err != nil {
			log.Printf("suppression(learn): could not record demotion evidence for %s: %v", a.ExternalRef, err)
		}
	}
	log.Printf("suppression(learn): REVERSED — learned schedule %q on %s suppressed %s but the boot was %q; row demoted to observing and the incident reopened",
		key.Cron, a.Host, a.ExternalRef, res.BootReason)
	return escalateReversal(a, "learned scheduled-reboot suppression reversed: the boot was not clean — pattern demoted to observing")
}

// escalateReversal builds the reversal decision. Its outcome is OutcomeEscalate, so every downstream
// consumer treats the incident as investigated.
func escalateReversal(a suppression.Alert, reason string) suppression.Decision {
	return suppression.Decision{
		Outcome: suppression.OutcomeEscalate, Phase: suppression.PhaseScheduledReboot,
		Reason: reason, ExternalRef: a.ExternalRef,
		Signals: map[string]string{"schedule_source": suppression.SourceLearned.String(), "reversed": "true"},
	}
}

// DemotePass is the SCHEDULED unlearning sweep (spec/005 REQ-411): it turns accumulated suppression-miss
// evidence into durable org-global analysis-only demotion rows, and re-asserts the demotion on every live
// learned row of each proven host. The in-path reversal already stopped the exact row that misfired; this
// pass is what makes the lesson STICK — the demotion row is what the chain consults on every later
// decision, it carries its own 30-day expiry (REQ-304), and it lands on the audit spine.
//
// It returns the number of demotion rows written. A nil evidence store or demoter makes it a no-op.
func (g *LiveSuppressGate) DemotePass(ctx context.Context, d *coregov.Demoter, lookback time.Duration, now time.Time) (int, error) {
	if g.Evidence == nil || d == nil {
		return 0, nil
	}
	evidence, err := g.Evidence.Since(ctx, now.Add(-lookback))
	if err != nil {
		return 0, err
	}
	rows, err := d.EvaluateEvidence(ctx, evidence, now)
	for _, r := range rows {
		if n := g.Learn.DemoteHost(r.Tuple.Host); n > 0 {
			log.Printf("suppression(demote pass): %d learned schedule(s) on %s returned to observing — %s", n, r.Tuple.Host, r.Reason)
		}
	}
	return len(rows), err
}

// BootReasonOf extracts the recorded boot reason from a validated incident envelope: an explicit provider
// label wins, otherwise the alert summary is scanned. It is DATA, never control flow (INV-08) — the only
// thing it can do is decide whether a reboot may be LEARNED or a suppression CONFIRMED, and every
// unrecognized value fails to "not clean", which means "do not learn / do not confirm".
func BootReasonOf(env ingest.IncidentEnvelope) string {
	for _, k := range []string{"boot_reason", "reboot_reason", "last_boot_reason"} {
		if v := env.Labels[k]; v != "" {
			return v
		}
	}
	return env.Summary
}
