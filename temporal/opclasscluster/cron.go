// Package opclasscluster is the earned-catalog clustering pass (spec/028 REQ-2811/REQ-2812, epic TG-227
// plane 3, Stage 2). Once per interval it reads the shadow-proposal evidence journal, advances keys that
// clear the MECHANICAL recurrence bars, and expires keys that have gone silent.
//
// It is OBSERVE-ONLY in the strongest sense: it can move a candidate from observing to candidate to
// ratify_ready and no further. Ratification — the act that grants a capability — is an OPERATOR act in
// Stage 4. Nothing in this package writes the registry, the graduation ladder, or any actuation surface, so
// no bug here can manufacture a capability; the worst it can do is put a candidate in front of a human
// early, where the human is the gate.
//
// Shape: the finalizer/`calibrate` precedent — a Job with injected dependencies, one pure Run pass, and
// RunPeriodically that runs IMMEDIATELY and then on the interval. Per-item errors never abort the pass
// (one unparseable candidate must not stop the other nineteen from accruing evidence).
package opclasscluster

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/opclasscat"
)

// ErrIntakeStale is the DEAD-MAN refusal (REQ-2812): occurrence intake has stopped while sessions are
// still flowing, so this pass would compute candidacy over a silently truncated evidence set.
var ErrIntakeStale = errors.New("opclasscluster: occurrence intake stale while sessions flow")

// StaleIntakeAfter is the dead-man threshold. Deliberately generous relative to the injector/alert cadence:
// this fires on a BROKEN seam, never on a quiet estate.
const StaleIntakeAfter = 48 * time.Hour

// Liveness is the dead-man's two facts, and the whole point is WHERE they come from: both are computed
// from tables this cron does NOT write.
//
// The dead-judge lesson, paid twice: the LLM judge died and stayed dead for days because every signal that
// was supposed to notice was derived from the judge's own writes — no writes, no rows, no alarm, and a
// green process the whole time. A liveness check a component computes from its own output can only ever
// report "I am fine" or say nothing at all. So NewestOccurrence comes from the occurrence journal (written
// by the runner's shadow activity, not by this cron) and SessionsSince comes from session_triage (written
// by the runner). If the seam between them breaks, the two disagree and this pass refuses loudly.
type Liveness struct {
	NewestOccurrence time.Time // newest row in the occurrence journal (written by the runner)
	SessionsSince    int       // sessions recorded in the same window (written by the runner)
}

// ReadyResolver supplies the completeness facts the journal cannot know: the mechanically-assigned
// family/tier and the estate blast-radius coverage for a candidate's targets.
//
// Nil is FAIL-CLOSED and expected in Stage 2: with no resolver, coverage is 0, MeetsRatifyReady is false,
// and candidates simply stay candidates. Stage 3's world model supplies the estate walk; until it does, no
// incomplete dossier is ever presented to an operator as if it were complete.
type ReadyResolver func(ctx context.Context, c opclasscat.Candidate, occs []opclasscat.Occurrence) (opclasscat.ReadyInput, error)

// Job is one clustering pass.
type Job struct {
	Store  opclasscat.Store
	Ledger opclasscat.Ledger
	// Liveness supplies the dead-man's inputs. REQUIRED: a nil probe is itself a fail-closed refusal —
	// a cron that cannot prove its intake is alive must not compute candidacy.
	Liveness func(ctx context.Context, window time.Duration) (Liveness, error)
	// Ready is optional (see ReadyResolver): nil ⇒ no candidate advances to ratify_ready.
	Ready ReadyResolver
	// Now is injectable for the oracles; nil ⇒ time.Now().UTC().
	Now func() time.Time
}

// Result is what one pass did — counts only, so the log line is honest and greppable.
type Result struct {
	Scanned       int
	ToCandidate   int
	ToRatifyReady int
	Expired       int
	ItemErrors    int
	// Held names every candidate the ratify-ready gate is holding, WITH the legs it fails (TG-348).
	//
	// A count of held candidates is not actionable: measured 2026-08-06, all 8 on the deployed estate sat
	// at `observing`, and "one distinct-ref short" and "the blast-radius provider is not wired at all" were
	// the same row. Only the second is a deployment defect, and only the first is fixed by waiting.
	Held []HeldCandidate
}

// HeldCandidate is one candidate the gate is holding and why.
type HeldCandidate struct {
	Key     string
	OpClass string
	Gaps    []string
}

func (j Job) now() time.Time {
	if j.Now != nil {
		return j.Now().UTC()
	}
	return time.Now().UTC()
}

// Run performs one pass: dead-man first, then per-candidate evaluation.
//
// The dead-man runs BEFORE anything else and aborts the WHOLE pass. That ordering is the requirement: a
// pass that proceeded on stale intake would compute "this key only has two incidents" from a truncated
// journal and hold back a candidate that genuinely recurred — a silent false negative in the direction
// nobody notices, which is exactly how the judge stayed dead.
func (j Job) Run(ctx context.Context) (Result, error) {
	var res Result

	if j.Liveness == nil {
		return res, fmt.Errorf("%w: no liveness probe wired", ErrIntakeStale)
	}
	live, err := j.Liveness(ctx, StaleIntakeAfter)
	if err != nil {
		return res, fmt.Errorf("liveness probe: %w", err)
	}
	now := j.now()
	// Sessions flowing + no fresh occurrences = the seam is broken. A genuinely quiet estate (no sessions)
	// is NOT stale — silence with no work is honest silence.
	if live.SessionsSince > 0 && (live.NewestOccurrence.IsZero() || now.Sub(live.NewestOccurrence) > StaleIntakeAfter) {
		age := "never"
		if !live.NewestOccurrence.IsZero() {
			age = now.Sub(live.NewestOccurrence).Round(time.Hour).String()
		}
		return res, fmt.Errorf("%w: newest occurrence %s old while %d session(s) flowed in the window",
			ErrIntakeStale, age, live.SessionsSince)
	}

	cands, err := j.Store.LiveCandidates(ctx)
	if err != nil {
		return res, fmt.Errorf("live candidates: %w", err)
	}
	res.Scanned = len(cands)

	for _, c := range cands {
		// Per-item isolation: one bad candidate never aborts the pass (the finalizer contract).
		if err := j.advance(ctx, c, now, &res); err != nil {
			res.ItemErrors++
			log.Printf("opclasscluster: candidate %s: %v (pass continues)", short(c.CandidateKey), err)
		}
	}
	return res, nil
}

func (j Job) advance(ctx context.Context, c opclasscat.Candidate, now time.Time, res *Result) error {
	occs, err := j.Store.Occurrences(ctx, c.CandidateKey, now.Add(-opclasscat.EvidenceWindow))
	if err != nil {
		return fmt.Errorf("occurrences: %w", err)
	}
	ev := opclasscat.Summarize(occs)

	// Expiry first: a key with no evidence in the silence window is done, whatever status it holds. The
	// key stays re-observable — expiry retires the ROW, never the possibility.
	if !c.LastSeenAt.IsZero() && now.Sub(c.LastSeenAt) > opclasscat.SilenceExpiry {
		if _, err := opclasscat.Transition(ctx, j.Store, j.Ledger, c, opclasscat.StatusExpired,
			fmt.Sprintf("no occurrence in %s (last seen %s)", opclasscat.SilenceExpiry, c.LastSeenAt.Format(time.RFC3339))); err != nil {
			return fmt.Errorf("expire: %w", err)
		}
		res.Expired++
		return nil
	}

	switch c.Status {
	case opclasscat.StatusObserving:
		if !opclasscat.MeetsCandidacy(ev) {
			return nil
		}
		if _, err := opclasscat.Transition(ctx, j.Store, j.Ledger, c, opclasscat.StatusCandidate,
			fmt.Sprintf("%d distinct incidents across %d host(s) over %s, mean confidence %.2f",
				ev.DistinctRefs, ev.DistinctHosts, ev.Span.Round(time.Hour), ev.MeanConfidence)); err != nil {
			return fmt.Errorf("to candidate: %w", err)
		}
		res.ToCandidate++

	case opclasscat.StatusCandidate:
		if j.Ready == nil {
			// FAIL-CLOSED, AND SAY SO (TG-348). No resolver ⇒ no dossier ⇒ no operator surface. This is
			// the DEPLOYED state — measured 2026-08-06, every candidate sat below the gate with nothing
			// reporting why — and it is the one branch that can never resolve on its own: a candidate
			// short on evidence gets there eventually, a candidate with no blast-radius walk never does.
			// Returning silently here made those two indistinguishable, which is the whole finding.
			res.Held = append(res.Held, HeldCandidate{
				Key: c.CandidateKey, OpClass: c.OpClass,
				Gaps: []string{"no blast-radius resolver wired (Stage 3) — this candidate cannot reach an operator at all"},
			})
			return nil
		}
		in, err := j.Ready(ctx, c, occs)
		if err != nil {
			return fmt.Errorf("ready resolver: %w", err)
		}
		// STAMP THE ROW EVEN BELOW THE GATE (TG-227 blocker 4's stamp half). The console queue renders
		// auto_barred/family/tier straight off the candidate row; stamping only at the ratify_ready
		// transition would leave every below-gate candidate showing the fail-closed default (barred=true,
		// no family) as if the screen had never run — the queue would be lying about screened candidates.
		// A non-status write: Transition stays the only path that moves Status.
		if in.AutoBarredStamped && (c.AutoBarred != in.AutoBarred || c.Family != in.Family || c.Tier != in.Tier) {
			c.AutoBarred, c.Family, c.Tier = in.AutoBarred, in.Family, in.Tier
			if uerr := j.Store.UpdateCandidate(ctx, c); uerr != nil {
				return fmt.Errorf("persist screen stamp: %w", uerr)
			}
		}
		if gaps := opclasscat.RatifyReadyGaps(ev, in); len(gaps) > 0 {
			// WHY the candidate is held, not merely THAT it is (TG-348). Measured 2026-08-06: all 8
			// candidates sat at `observing` with nothing distinguishing "one distinct-ref short" from
			// "the blast-radius provider is not wired at all" — and only the second is a deployment
			// defect. The gate stays byte-identical; this reports its reasoning.
			res.Held = append(res.Held, HeldCandidate{Key: c.CandidateKey, OpClass: c.OpClass, Gaps: gaps})
			return nil
		}
		if _, err := opclasscat.Transition(ctx, j.Store, j.Ledger, c, opclasscat.StatusRatifyReady,
			fmt.Sprintf("dossier complete: %d refs, family %s, tier %s, blast radius %.0f%% covered",
				ev.DistinctRefs, in.Family, in.Tier, in.BlastRadiusCoverage*100)); err != nil {
			return fmt.Errorf("to ratify_ready: %w", err)
		}
		res.ToRatifyReady++
	}
	return nil
}

func short(k string) string {
	if len(k) > 12 {
		return k[:12]
	}
	return k
}

// RunPeriodically performs one pass IMMEDIATELY and then one every `every` until ctx is done. It blocks;
// callers run it in a goroutine.
//
// The immediate pass is deliberate (the calibrate precedent): a bare `for range t.C` leaves the whole
// clustering plane silent for a full interval after every worker start, and "silent" is indistinguishable
// from "nothing recurred" on a surface whose entire job is to notice recurrence.
func RunPeriodically(ctx context.Context, j Job, every time.Duration, onErr func(error)) {
	pass := func() {
		res, err := j.Run(ctx)
		if err != nil {
			if onErr != nil {
				onErr(err)
			}
			return
		}
		if res.ToCandidate > 0 || res.ToRatifyReady > 0 || res.Expired > 0 {
			log.Printf("opclasscluster: scanned %d, +%d candidate, +%d ratify-ready, %d expired, %d item error(s)",
				res.Scanned, res.ToCandidate, res.ToRatifyReady, res.Expired, res.ItemErrors)
		}
		// WHY the queue is empty, on every pass that holds something (TG-348). Logged unconditionally
		// rather than beside the transitions above: a pass that promotes NOTHING is exactly the pass whose
		// reasoning an operator needs, and it is the one the condition above stays silent for.
		//
		// An operator reading "0 ratify-ready" cannot tell a candidate that needs two more incidents from
		// one whose blast-radius provider was never wired. The first is patience; the second is a
		// deployment defect that will never resolve on its own.
		for _, h := range res.Held {
			log.Printf("opclasscluster: HELD %s (%s) — %s", h.OpClass, short(h.Key), strings.Join(h.Gaps, "; "))
		}
	}
	pass()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass()
		}
	}
}
