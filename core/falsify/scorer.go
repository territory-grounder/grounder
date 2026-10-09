// Package falsify is the verify-time FALSIFIABILITY WRITEBACK: the production caller that finally scores a
// committed infragraph prediction against OBSERVED reality, closing the predict → verdict → score chain the
// Phase-2 readiness review flagged as having zero production callers (so SignalRatio / the grounding
// scorecard was degenerate — TG had never scored one real prediction).
//
// It is MEASUREMENT ONLY and works regardless of the mutation gate: a prediction is committed BEFORE any
// action (by the gate) and scored AFTER a post-incident observation window elapses. That window is LEARNED
// per edge from observed cascade latency — max(900s, 2×p95), capped (window.go, REQ-110, TG-220) — not a
// constant: scoring a 15-minute cascade in a fixed 10-minute window records a miss that never happened, and
// these numbers are the instrument the predecessor head-to-head reads. TWO POPULATIONS flow through, and the
// scorer keeps their meanings apart (the C4 adjudication repair):
//
//   - Every due prediction — executed or not — gets the FALSIFIABILITY writeback: the confusion-matrix score
//     (tp/fp/fn + control_tp/control_fp, predict.ScoreControl vs the degree-preserving shuffled control)
//     written back onto the prediction row (the SOLE verify-time write — the immutable prediction identity is
//     never touched), and the batch's real-vs-control totals accumulate one windowed infragraph_cascade_stats
//     row (INV-22 — the over-prediction gate: a control that captures ≥ half the real cascades means the
//     graph adds no signal). This measurement is the POINT of this package and is symmetric between the real
//     prediction and its control, so ambient noise cannot tilt it.
//   - ONLY a NEVER-EXECUTED prediction additionally gets a FORECAST verdict (prediction_verdict): the
//     world-model grade "TG predicted Y would happen around this incident; did it?", authored by the
//     deterministic verifier WITH the commit-time baseline (the (host,rule) pairs and open-incident hosts
//     already firing at CommittedAt) and the estate-derived cross-site scope — without those, diffing a
//     forecast against the ambient estate can only ever read deviation (the live 19/19-deviation table this
//     repair closes). An EXECUTED prediction's adjudication is an ACTION outcome and belongs exclusively to
//     the interceptor/reconcile lane (core/actuate, action_verdict, the graduation ladder); this scorer
//     writes NO verdict for it, so a forecast grade can never feed op-class graduation or demotion.
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

// DuePrediction is a committed-but-unscored prediction past the observation-window FLOOR — a CANDIDATE for
// scoring. Whether it is actually due is decided in the scorer against its own LEARNED window (the durable
// read cannot know it: it depends on per-edge observed latency — see window.go / REQ-110), so a candidate
// whose slowest claimed edge has not had time to manifest is deferred, not scored.
// CommittedAt anchors that decision, the window aggregate the batch accumulates, AND
// the commit-time baseline the forecast verdict is computed against. Executed reports whether the bound
// action ever actually ran (a row in the per-execution record, action_execution): an executed prediction is
// scored for falsifiability only — its adjudication is an ACTION outcome owned by the interceptor lane, and
// grading it here as a forecast would be the category error this field exists to prevent.
type DuePrediction struct {
	Record      predict.PredictionRecord
	CommittedAt time.Time
	Executed    bool
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
// predates olderThan, oldest first, up to limit. The scorer passes the observation-window FLOOR here — the
// shortest window any prediction can have — so no candidate is missed and the per-prediction learned window
// is applied in Go. The pgx db.FalsifiabilityStore is the production implementation; MemStore is the oracle
// twin.
type UnscoredReader interface {
	DueForScoring(ctx context.Context, olderThan time.Time, limit int) ([]DuePrediction, error)
}

// ScoreWriter writes the verify-time score columns back onto a committed prediction (keyed by plan_hash). It
// is IDEMPOTENT: it updates only rows still unscored (tp IS NULL), returning whether a row was actually
// scored — so a concurrent or repeated run never double-counts a prediction into the cascade window.
type ScoreWriter interface {
	WriteScore(ctx context.Context, planHash string, s Score) (bool, error)
}

// VerdictWriter persists the mechanical FORECAST verdict for a scored, never-executed prediction. INV-10:
// the deterministic verifier (verify.ComputeVerdictDetailScoped) is the sole author; this only durably
// records what it produced. Append-only, first-wins per action_id. The pgx db.PredictionVerdictStore
// (prediction_verdict, migration 0042) satisfies it — NEVER wire db.VerdictStore here: action_verdict means
// "TG did X and the estate responded", it has exactly one writer (the interceptor), and a forecast grade
// routed into it would feed op-class graduation/demotion with world-model noise (the C4 category error).
// Optional (nil ⇒ the score columns still land; no forecast verdict rows are recorded).
type VerdictWriter interface {
	Commit(ctx context.Context, actionID, planHash, targetHost, site string, v safety.Verdict) error
}

// BaselineReader returns the COMMIT-TIME baseline for a prediction — the (host,rule) pairs already alerting
// and the hosts already holding an OPEN incident as of asOf (the prediction's CommittedAt) — read back from
// the durable ingest ledger (db.FalsifyBaseline over ingest_alert/ingest_transition), which is anchored by
// construction: nothing that fired AFTER the commit can launder into it. ok=false means the ledger could not
// be read; the scorer then SKIPS the prediction (left unscored, retried later) rather than adjudicate
// without a baseline — the same refuse-to-adjudicate discipline as the interceptor's baseline gate
// (REQ-1228): a forecast verdict diffed against the ambient estate with no baseline can only manufacture
// deviations, which is precisely the defect this seam repairs.
type BaselineReader func(ctx context.Context, asOf time.Time) (pairs []verify.ObservedAlert, openHosts map[string]bool, ok bool)

// CascadeStatsWriter appends one windowed falsifiability aggregate (INV-22 over-prediction gating). Optional.
type CascadeStatsWriter interface {
	AppendWindow(ctx context.Context, w CascadeWindow) error
}

// Observer reads the alerts OBSERVED in the post-incident window for a (targetHost, site) — the SAME live
// surface (LibreNMS active alerts) that already feeds the interceptor's ComputeVerdict, reused here on the
// read-only path so scoring never depends on mutation being ON. It returns (alerts, ok): ok=true with a
// (possibly empty) slice is a REAL observation (a quiet post-state = no cascade); ok=false means the surface
// could NOT be read (a monitoring outage), in which case the scorer SKIPS this prediction (leaves it unscored
// to retry) rather than score a vacuous `match` on zero evidence — the same fail-closed contract the verifier
// and ClearObserve use (TG-182).
type Observer func(ctx context.Context, targetHost, site string) ([]verify.ObservedAlert, bool)

// Scorer is the verify-time falsifiability writeback. All collaborators are injected so the oracle drives it
// with in-memory fakes (CI has no Postgres) and production wires the pgx stores + the live observer.
type Scorer struct {
	Unscored UnscoredReader
	Scores   ScoreWriter
	// ForecastVerdicts is the FORECAST-verdict sink (prediction_verdict) — written ONLY for never-executed
	// predictions, and ONLY when the commit-time Baseline was established. Optional.
	ForecastVerdicts VerdictWriter
	CascadeStats     CascadeStatsWriter // optional
	Discovery        DiscoveryWriter    // optional — captures each forecast-graded deviation into the rolling discovery corpus
	Observe          Observer
	// Baseline reads the commit-time baseline (pairs + open-incident hosts at CommittedAt) from the durable
	// ingest ledger. A wired-but-failing read SKIPS the prediction (retried later). Nil means the deployment
	// has no durable history to anchor a baseline on: the falsifiability score still lands (it is
	// noise-symmetric), but NO forecast verdict is authored — a verdict computed outside a baseline is the
	// manufactured-deviation class this scorer exists to stop writing.
	Baseline BaselineReader
	// HostSite is the estate-derived site vocabulary for the verdict's coincidental-cross-site filter
	// (spec/002 REQ-107; estate.Graph.SiteOf in production). Optional — nil excludes nothing (fail closed).
	HostSite verify.SiteAuthority
	// Latency is the durable per-edge cascade-latency evidence the observation window is LEARNED from
	// (spec/002 REQ-110, TG-220). Optional: nil — or a read that reports ok=false — leaves every edge on the
	// WindowFloor, i.e. the fixed-window behavior with a 900s floor. See window.go for the ported rule.
	Latency LatencyReader
	// WindowFloor is the MINIMUM time AFTER a prediction is committed before it is scoreable — the cascade
	// must have had time to manifest — and the window an edge with no observed history gets. WindowCap bounds
	// the learned window so one outlier cannot strand a prediction unscored. LatencyLookback bounds how far
	// back the durable latency evidence is read. Zero values take the window.go defaults (900s / 2h / 14d).
	// Batch bounds how many predictions one pass considers. Now overrides the clock (tests).
	WindowFloor     time.Duration
	WindowCap       time.Duration
	LatencyLookback time.Duration
	Batch           int
	Now             func() time.Time
}

// Result reports what one ScoreDue pass did — surfaced for the worker log and asserted by the oracle.
type Result struct {
	Scored  int
	Skipped int // predictions left unscored this pass because the post-state OR the commit-time baseline was unreadable (retried later)
	// Deferred counts predictions past the WindowFloor whose own LEARNED window (max(floor, 2×p95) over their
	// slowest claimed edge) has not elapsed yet — deliberately left unscored so a slow cascade is adjudicated
	// on what actually happened rather than recorded as a miss for being slower than a constant (TG-220).
	// Distinct from Skipped: nothing failed, the evidence is simply not in yet. Every deferral ends within
	// WindowCap, so a deferred prediction occupies the oldest-first Batch for a BOUNDED number of passes and
	// cannot starve newer ones indefinitely.
	Deferred int
	// WidestWindow is the largest learned observation window this pass applied — log-only observability for
	// "how far has the estate's observed propagation lag pushed the window past the floor".
	WidestWindow time.Duration
	Executed     int // of Scored: predictions whose action really ran — falsifiability-scored only, NO forecast verdict (the action lane owns their adjudication)
	SumRealTP    int
	SumControlTP int
	Deviations   int    // FORECAST deviations this pass authored (never-executed predictions only)
	VerdictErrs  int    // forecast-verdict-write blips (best-effort; the score is already durable)
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

// ScoreDue scores every due prediction once. DUE means past the prediction's OWN LEARNED observation window
// (REQ-110) — max(WindowFloor, 2×p95 observed latency) over its slowest claimed edge, capped by WindowCap —
// not merely past a constant: a candidate inside its window is DEFERRED (counted, left unscored, retried
// next pass), because scoring a cascade that has not finished records a miss that never happened.
//
// It is best-effort and side-effect measurement: it NEVER mutates the estate, never consults the mutation
// gate, and a partial failure surfaces an error for the caller to retry next tick rather than corrupting
// state (each score write is atomic + idempotent). Inert (returns zero, no error) when its required
// collaborators are unwired — honest zeros, never a panic.
func (s *Scorer) ScoreDue(ctx context.Context) (Result, error) {
	var res Result
	if s.Unscored == nil || s.Scores == nil || s.Observe == nil {
		return res, nil // not wired — measurement is inert, never blocks
	}
	now := s.now()
	// The durable read is bounded by the FLOOR — the shortest window any prediction can have — so a candidate
	// is never missed; each candidate's OWN learned window is then applied below. (Filtering at the widest
	// possible window instead would delay every fast-edge prediction to the slowest edge in the estate.)
	due, err := s.Unscored.DueForScoring(ctx, now.Add(-s.windowFloor()), s.batch())
	if err != nil {
		return res, err
	}
	// ONE durable latency read per pass, over exactly the target hosts this batch claims cascades from. An
	// unreadable read yields no samples, so every edge falls back to the floor — a DB blip widens nothing and
	// shortens nothing.
	lat := s.latencies(ctx, due, now)
	var windowStart time.Time
	for _, d := range due {
		pred := d.Record.Prediction
		// THE LEARNED WINDOW (REQ-110). max(floor, 2×p95) over this prediction's slowest claimed edge, capped.
		// Until it elapses the prediction is DEFERRED, not scored: adjudicating a 15-minute cascade at 10
		// minutes records a miss that never happened, which is precisely the bias this replaces.
		w := PredictionWindow(pred.TargetHost, pred.PredictedHosts, lat, s.windowFloor(), s.windowCap())
		if w > res.WidestWindow {
			res.WidestWindow = w
		}
		if now.Sub(d.CommittedAt) < w {
			res.Deferred++
			continue
		}
		observed, ok := s.Observe(ctx, pred.TargetHost, pred.Site)
		if !ok {
			// Fail-closed (TG-182): the post-state could not be read this round. SKIP — leave the prediction
			// unscored so it is retried on a later pass when the surface returns, rather than scoring a vacuous
			// `match` (empty observation) that would falsely record the prediction as held on zero evidence.
			res.Skipped++
			continue
		}
		// The COMMIT-TIME baseline, anchored at CommittedAt from the durable ingest ledger. A wired-but-failed
		// read SKIPS the whole prediction (the score write below would mark the row tp-non-null and it would
		// never be re-picked, so a verdict could never be authored later — skipping keeps it retryable). A nil
		// seam (no durable history) proceeds baseline-less: the noise-symmetric falsifiability score still
		// lands, but baselineOK=false withholds the forecast verdict below.
		var basePairs []verify.ObservedAlert
		var baseHosts map[string]bool
		baselineOK := false
		if s.Baseline != nil {
			if basePairs, baseHosts, baselineOK = s.Baseline(ctx, d.CommittedAt); !baselineOK {
				res.Skipped++
				continue
			}
		}
		// The deterministic pair: the falsifiability score (real vs degree-preserving control) and the typed
		// verify.VerdictDetail — the mechanical verdict AND its structured breakdown (surprise hosts / rule
		// mismatches) in ONE verifier pass, computed WITH the commit-time baseline and the estate-derived
		// cross-site scope (spec/002 REQ-106/REQ-107/REQ-108) so a pre-existing ambient alert or a
		// proven-other-site flap no longer reads as this prediction's failed cascade. We consume the typed
		// detail here rather than re-diffing the prediction against the observation to rediscover which hosts
		// surprised. (The falsifiability confusion matrix stays with ScoreControl and takes NO baseline: it is
		// SYMMETRIC between the real prediction and its shuffled control, so ambient noise hits both sides
		// equally and the INV-22 control ratio stays an honest tripwire — baselining it would launder exactly
		// the noise the control exists to expose.)
		cs := predict.ScoreControl(d.Record, observed)
		detail := verify.ComputeVerdictDetailScoped(pred, observed, basePairs, baseHosts, s.HostSite)
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
		res.Scored++
		res.SumRealTP += cs.RealTP
		res.SumControlTP += cs.ControlTP
		if windowStart.IsZero() || d.CommittedAt.Before(windowStart) {
			windowStart = d.CommittedAt
		}
		// THE SINK SPLIT (C4 defect 2). An EXECUTED prediction stops here: its adjudication is an ACTION
		// outcome — the interceptor authored (or deliberately withheld) it against the real pre-execution
		// baseline, and the graduation ladder feeds off that lane alone (TG-184: one writer per meaning).
		// Re-grading it here as a forecast would hand a second, weaker-baselined author a write path into the
		// op-class record. The falsifiability score above is still wanted — the graph's accuracy is measurable
		// on every prediction — but no verdict of any kind is authored for it in this lane.
		if d.Executed {
			res.Executed++
			continue
		}
		// FORECAST verdict — never-executed predictions only, and only with an ESTABLISHED commit-time
		// baseline: "what will cascade IF this action runs" diffed against the ambient estate without one can
		// only produce deviation (the 19/19-deviation prediction_verdict table this repair closes). A DEVIATION
		// is never-auto by construction: verify.AutoResolvable(deviation) is false. Best-effort: the
		// falsifiability score is already durable and the row is now tp-non-null (so it will not be re-picked),
		// so a verdict blip must not re-drive the whole pass — it is counted, not fatal.
		if !baselineOK {
			continue // no durable baseline seam wired — measurement only; a verdict outside a baseline is the manufactured-deviation class
		}
		if s.ForecastVerdicts != nil {
			if verr := s.ForecastVerdicts.Commit(ctx, pred.ActionID, pred.PlanHash, pred.TargetHost, pred.Site, verdict); verr != nil {
				res.VerdictErrs++
			}
		}
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

// latencies reads the per-edge observed cascade latencies backing this pass's learned windows, for exactly
// the target hosts the due batch claims cascades FROM. Returns an empty (never nil-unsafe) map when the seam
// is unwired or the durable read failed — both resolve every edge to the floor, which is the fail-safe
// direction: an unreadable ledger must never SHORTEN a window and manufacture misses.
func (s *Scorer) latencies(ctx context.Context, due []DuePrediction, now time.Time) map[CascadeEdge][]time.Duration {
	if s.Latency == nil || len(due) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	primaries := make([]string, 0, len(due))
	for _, d := range due {
		h := d.Record.Prediction.TargetHost
		if h == "" {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		primaries = append(primaries, h)
	}
	if len(primaries) == 0 {
		return nil
	}
	sort.Strings(primaries) // deterministic query input
	lat, ok := s.Latency(ctx, primaries, now.Add(-s.latencyLookback()))
	if !ok {
		return nil // unreadable — every edge falls back to the floor
	}
	return lat
}

// windowFloor is the minimum observation window (and the window for an edge with no observed history).
func (s *Scorer) windowFloor() time.Duration {
	if s.WindowFloor <= 0 {
		return DefaultWindowFloor
	}
	return s.WindowFloor
}

// windowCap bounds the learned window so one outlier observation cannot strand a prediction unscored. A cap
// configured BELOW the floor is a misconfiguration and the floor wins: this clamp may only ever widen.
func (s *Scorer) windowCap() time.Duration {
	floor := s.windowFloor()
	c := s.WindowCap
	if c <= 0 {
		c = DefaultWindowCap
	}
	if c < floor {
		c = floor
	}
	return c
}

// latencyLookback bounds how far back the durable ledger is read for latency evidence.
func (s *Scorer) latencyLookback() time.Duration {
	if s.LatencyLookback <= 0 {
		return DefaultLatencyLookback
	}
	return s.LatencyLookback
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
