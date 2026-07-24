// Package falsify is the verify-time FALSIFIABILITY WRITEBACK: the production caller that finally scores a
// committed infragraph prediction against OBSERVED reality, closing the predict → verdict → score chain the
// Phase-2 readiness review flagged as having zero production callers (so SignalRatio / the grounding
// scorecard was degenerate — TG had never scored one real prediction).
//
// It is MEASUREMENT ONLY and works regardless of the mutation gate: a prediction is committed BEFORE any
// action (by the gate) and scored AFTER a post-incident observation window elapses — no execution required.
// For each committed-but-unscored prediction whose window has passed, the deterministic verifier
// (verify.ComputeVerdictDetail) and the falsifiability scorer (predict.ScoreControl) diff the prediction and its
// degree-preserving shuffled negative control against the live observed alerts, then:
//   - the confusion-matrix score (tp/fp/fn + control_tp/control_fp) is written back onto the prediction row
//     (the SOLE verify-time write — the immutable prediction identity is never touched);
//   - the mechanical verdict is persisted (INV-10 — ComputeVerdict is the sole author; a DEVIATION verdict is
//     never-auto BY CONSTRUCTION, verify.AutoResolvable(deviation) is false);
//   - the batch's real-vs-control totals accumulate one windowed infragraph_cascade_stats row (INV-22 — the
//     over-prediction gate: a control that captures ≥ half the real cascades means the graph adds no signal).
//
// Provenance: [O] INV-10 (deterministic verifier is the sole verdict writer; deviation never auto-resolves),
// INV-22 (falsifiable-by-construction — every prediction carries its degree-preserving control) · [F] the
// predecessor's blast-radius precision / cascade_stats scoring, re-expressed under the typed spine. Mutation
// stays OFF — this scores, it never actuates.
package falsify

import (
	"context"
	"sort"
	"time"

	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// Score is the verify-time confusion matrix written back onto a committed prediction row. tp/fp/fn are the
// real prediction's cells; control_tp/control_fp are its degree-preserving shuffled control's. These are the
// ONLY columns the verifier writes — the prediction identity committed before the poll is immutable.
type Score struct {
	TP        int
	FP        int
	FN        int
	ControlTP int
	ControlFP int
}

// DuePrediction is a committed-but-unscored prediction whose observation window has elapsed — ready to be
// scored against the post-incident estate. CommittedAt anchors the window aggregate the batch accumulates.
type DuePrediction struct {
	Record      predict.PredictionRecord
	CommittedAt time.Time
}

// CascadeWindow is one appended infragraph_cascade_stats row: the batch's real-vs-control totals and the
// INV-22 falsifiability verdict over the window (control_ratio = control_tp / max(real_tp,1); <= the ceiling
// means the real topology beat its same-shape random control by the required margin).
type CascadeWindow struct {
	Start        time.Time
	End          time.Time
	RealTP       int
	ControlTP    int
	ControlRatio float64
	Falsifiable  bool
}

// UnscoredReader lists committed predictions the verifier has NOT yet scored (tp IS NULL) whose commit time
// predates olderThan (the observation window has elapsed), oldest first, up to limit. The pgx
// db.FalsifiabilityStore is the production implementation; MemStore is the oracle twin.
type UnscoredReader interface {
	DueForScoring(ctx context.Context, olderThan time.Time, limit int) ([]DuePrediction, error)
}

// ScoreWriter writes the verify-time score columns back onto a committed prediction (keyed by plan_hash). It
// is IDEMPOTENT: it updates only rows still unscored (tp IS NULL), returning whether a row was actually
// scored — so a concurrent or repeated run never double-counts a prediction into the cascade window.
type ScoreWriter interface {
	WriteScore(ctx context.Context, planHash string, s Score) (bool, error)
}

// VerdictWriter persists the mechanical verdict for a scored prediction. INV-10: the deterministic verifier
// (verify.ComputeVerdictDetail) is the sole author; this only durably records what it produced. Append-only,
// first-wins per action_id. The pgx db.VerdictStore satisfies it. Optional (nil ⇒ the verdict rides only the
// prediction's score columns and the console verdict distribution stays as the interceptor writes it).
type VerdictWriter interface {
	Commit(ctx context.Context, actionID, planHash, targetHost, site string, v safety.Verdict) error
}

// CascadeStatsWriter appends one windowed falsifiability aggregate (INV-22 over-prediction gating). Optional.
type CascadeStatsWriter interface {
	AppendWindow(ctx context.Context, w CascadeWindow) error
}

// Observer reads the alerts OBSERVED in the post-incident window for a (targetHost, site) — the SAME live
// surface (LibreNMS active alerts) that already feeds the interceptor's ComputeVerdict, reused here on the
// read-only path so scoring never depends on mutation being ON. It returns a non-nil (possibly empty) slice:
// a quiet post-state is a real observation (no cascade), never a nil that would make every prediction a
// vacuous match.
type Observer func(ctx context.Context, targetHost, site string) []verify.ObservedAlert

// Scorer is the verify-time falsifiability writeback. All collaborators are injected so the oracle drives it
// with in-memory fakes (CI has no Postgres) and production wires the pgx stores + the live observer.
type Scorer struct {
	Unscored     UnscoredReader
	Scores       ScoreWriter
	Verdicts     VerdictWriter      // optional
	CascadeStats CascadeStatsWriter // optional
	Discovery    DiscoveryWriter    // optional — captures each scored deviation into the rolling discovery corpus
	Observe      Observer
	// Window is how long AFTER a prediction is committed before it is scoreable — the cascade must have had
	// time to manifest. Batch bounds how many predictions one pass scores. Now overrides the clock (tests).
	Window time.Duration
	Batch  int
	Now    func() time.Time
}

// Result reports what one ScoreDue pass did — surfaced for the worker log and asserted by the oracle.
type Result struct {
	Scored       int
	SumRealTP    int
	SumControlTP int
	Deviations   int
	VerdictErrs  int    // verdict-write blips (best-effort; the score is already durable)
	CascadeErr   string // a cascade-window append blip (best-effort)
	// DiscoveryCaptured is how many NEWLY-seen deviation signatures this pass captured into the discovery
	// corpus; DiscoveryErrs counts capture blips. Both are ADDITIVE, SIDE-EFFECT-FREE observability: capture
	// writes to a separate holding area and NEVER touches the prediction row, the verdict, or the confusion
	// matrix — a capture blip is counted, never fatal (the durable score+verdict already landed).
	DiscoveryCaptured int
	DiscoveryErrs     int
	// SurpriseHosts is the sorted, deduplicated union of the surprise hosts across this pass's DEVIATION
	// verdicts — read straight off the typed verify.VerdictDetail (the single verifier pass), never
	// recomputed here. Log-only observability ("which hosts diverged from the model this pass"); the durable
	// signal is the per-row confusion matrix. Empty when nothing deviated.
	SurpriseHosts []string
}

// ScoreDue scores every due prediction once. It is best-effort and side-effect measurement: it NEVER mutates
// the estate, never consults the mutation gate, and a partial failure surfaces an error for the caller to
// retry next tick rather than corrupting state (each score write is atomic + idempotent). Inert (returns
// zero, no error) when its required collaborators are unwired — honest zeros, never a panic.
func (s *Scorer) ScoreDue(ctx context.Context) (Result, error) {
	var res Result
	if s.Unscored == nil || s.Scores == nil || s.Observe == nil {
		return res, nil // not wired — measurement is inert, never blocks
	}
	now := s.now()
	due, err := s.Unscored.DueForScoring(ctx, now.Add(-s.window()), s.batch())
	if err != nil {
		return res, err
	}
	var windowStart time.Time
	for _, d := range due {
		pred := d.Record.Prediction
		observed := s.Observe(ctx, pred.TargetHost, pred.Site)
		// The deterministic pair: the falsifiability score (real vs degree-preserving control) and the typed
		// verify.VerdictDetail — the mechanical verdict AND its structured breakdown (surprise hosts / rule
		// mismatches) in ONE verifier pass. We consume the typed detail here rather than re-diffing the
		// prediction against the observation to rediscover which hosts surprised. (The falsifiability confusion
		// matrix stays with ScoreControl: it applies a cross-site noise filter the verdict deliberately does NOT
		// — a surprise host is always a deviation regardless of its site label — so the two are distinct
		// measurements, not a duplication.)
		cs := predict.ScoreControl(d.Record, observed)
		detail := verify.ComputeVerdictDetail(pred, observed)
		verdict := detail.Verdict
		updated, werr := s.Scores.WriteScore(ctx, pred.PlanHash, Score{
			TP: cs.RealTP, FP: cs.RealFP, FN: cs.RealFN, ControlTP: cs.ControlTP, ControlFP: cs.ControlFP,
		})
		if werr != nil {
			return res, werr
		}
		if !updated {
			continue // already scored (a concurrent pass won the atomic update) — never double-count
		}
		// Persist the verdict. A DEVIATION is never-auto by construction: verify.AutoResolvable(deviation) is
		// false, and the append-only action_verdict row is the durable record any downstream gate consults.
		// Best-effort: the falsifiability score is already durable and the row is now tp-non-null (so it will
		// not be re-picked), so a verdict blip must not re-drive the whole pass — it is counted, not fatal.
		if s.Verdicts != nil {
			if verr := s.Verdicts.Commit(ctx, pred.ActionID, pred.PlanHash, pred.TargetHost, pred.Site, verdict); verr != nil {
				res.VerdictErrs++
			}
		}
		res.Scored++
		res.SumRealTP += cs.RealTP
		res.SumControlTP += cs.ControlTP
		if verdict == safety.VerdictDeviation {
			res.Deviations++
			// The typed detail already names WHICH hosts diverged — accumulate them for the worker log instead
			// of re-deriving the surprise set here.
			res.SurpriseHosts = append(res.SurpriseHosts, detail.SurpriseHosts...)
			// CAPTURE this live-scored misprediction into the rolling discovery corpus (the flywheel's source
			// set). Additive + side-effect-free on the gate: it writes to a SEPARATE holding area and never
			// touches the prediction row, the verdict, or the confusion matrix (all already durable above). A
			// capture blip is counted, never fatal — the deviation is already scored and will not be re-picked.
			if s.Discovery != nil {
				captured, derr := s.Discovery.Capture(ctx, DiscoveryRecord{
					ActionID: pred.ActionID, PlanHash: pred.PlanHash, PredictionHash: d.Record.PredictionHash,
					TargetHost: pred.TargetHost, Site: pred.Site, Verdict: verdict,
					SurpriseHosts: detail.SurpriseHosts, Mismatches: detail.Mismatches, Observed: observed,
					Score:       Score{TP: cs.RealTP, FP: cs.RealFP, FN: cs.RealFN, ControlTP: cs.ControlTP, ControlFP: cs.ControlFP},
					CommittedAt: d.CommittedAt, ObservedAt: now,
				})
				switch {
				case derr != nil:
					res.DiscoveryErrs++
				case captured:
					res.DiscoveryCaptured++
				}
			}
		}
		if windowStart.IsZero() || d.CommittedAt.Before(windowStart) {
			windowStart = d.CommittedAt
		}
	}
	res.SurpriseHosts = dedupeSorted(res.SurpriseHosts)
	// Accumulate ONE windowed cascade-stats row over exactly the predictions this pass newly scored (INV-22).
	if res.Scored > 0 && s.CascadeStats != nil {
		ratio := ControlRatio(res.SumRealTP, res.SumControlTP)
		if cerr := s.CascadeStats.AppendWindow(ctx, CascadeWindow{
			Start: windowStart, End: now,
			RealTP: res.SumRealTP, ControlTP: res.SumControlTP,
			ControlRatio: ratio, Falsifiable: ratio <= predict.ControlRatioCeiling,
		}); cerr != nil {
			res.CascadeErr = cerr.Error() // best-effort: the per-row scores are already durable
		}
	}
	return res, nil
}

// dedupeSorted returns xs sorted with duplicates removed (nil when empty) — a stable, log-friendly union of
// the per-deviation surprise-host slices the typed verdict detail already produced.
func dedupeSorted(xs []string) []string {
	if len(xs) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(xs))
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}

// ControlRatio is control_tp / max(real_tp, 1) — the same floor predict.ControlScore.Ratio applies, so a
// zero-signal real prediction reads as a full control failure rather than dividing by zero.
func ControlRatio(realTP, controlTP int) float64 {
	denom := realTP
	if denom < 1 {
		denom = 1
	}
	return float64(controlTP) / float64(denom)
}

func (s *Scorer) window() time.Duration {
	if s.Window <= 0 {
		return 10 * time.Minute
	}
	return s.Window
}

func (s *Scorer) batch() int {
	if s.Batch <= 0 {
		return 200
	}
	return s.Batch
}

func (s *Scorer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}
