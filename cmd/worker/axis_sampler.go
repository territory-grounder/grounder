package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/metrics"
)

// THE BENCHMARK AXES WERE COMPUTED BY A CLI AND BY NOTHING ELSE.
//
// core/db.AxisReadStore.Aggregate derives the whole measured picture — A1 detection recall, per-class
// detection, and the per-source detection-LATENCY distribution — and it had EXACTLY ONE caller in the entire
// tree: cmd/axisscore/main.go. So every one of those numbers existed only when a human remembered to run a
// command, which means none of them could be graphed, alerted on, or noticed changing.
//
// That gap is what makes TG's fastest detector invisible. Measured on the live estate: pve-liveness is the
// FIRST reporter on 70 device-down faults at ~34s median, against librenms at ~612s — an 18x improvement on
// the faults it catches. Its contribution to A1 RECALL is one fault (89.3% -> 89.6%), because librenms
// eventually reports almost everything too. Recall is simply not the axis this detector moves; LATENCY is, and
// latency had no operational surface at all. A capability whose only benefit is invisible to the metrics will
// be "optimised away" by the next person reading a dashboard.
//
// So this samples the EXISTING aggregate on a tick and publishes it. No new derivation, no second copy of the
// SQL — the numbers on /metrics are the same numbers axisscore prints, by construction, because they come from
// the same function.
//
// FAIL-QUIET BY DESIGN: this is a measurement path. A DB error logs and leaves the previous sample in place;
// it never blocks triage, never touches the chokepoint, and never fabricates a value. Before the first
// successful sample there are NO series at all rather than zeros — a zero recall would read as "TG detects
// nothing", the most alarming possible false statement about a healthy estate.
type axisSampler struct {
	mu      sync.RWMutex
	agg     db.AxisAgg
	sampled bool      // false until a read succeeds — gates every series
	at      time.Time // when the held sample was taken
	window  time.Duration
	// fals is the falsifiability axis (TG-192). Held separately with its OWN gate: Aggregate and
	// Falsifiability are different reads and either can fail alone, so one must not silence the other.
	fals       db.Falsifiability
	falsSample bool
	// cov is per-dimension judge coverage (TG-360), held under its OWN gate for the same reason as fals: a
	// coverage read failing must not silence the A1 numbers, and vice-versa.
	cov       []db.DimCoverage
	covJudged int
	covSample bool
	// lb is the loop-bypass guardrail (TG-191, epic TG-187), held under its OWN gate like fals/cov: a
	// guardrail read failing must not silence the A1 numbers, and vice-versa.
	lb       db.LoopBypass
	lbSample bool
}

// newAxisSampler returns an unarmed sampler: it holds nothing and emits nothing.
func newAxisSampler(window time.Duration) *axisSampler {
	return &axisSampler{window: window}
}

// sample refreshes the held aggregate. An error leaves the previous sample untouched.
func (s *axisSampler) sample(ctx context.Context, store *db.AxisReadStore, now time.Time) error {
	agg, err := store.Aggregate(ctx, now.Add(-s.window))
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.agg, s.sampled, s.at = agg, true, now
	s.mu.Unlock()

	// TG-192, and REQ-2524 applied to the axis it was never applied to. The falsifiability axis — TG's
	// rarest asset, the real-graph-beats-its-shuffled-control proof — was computed, stored per prediction
	// (infragraph_prediction.control_tp/control_fp) and aggregated with an honest denominator, and had
	// exactly ONE caller in the tree: cmd/axisscore. REQ-2524 says of precisely that shape: "a measurement
	// reachable only by a human running a command is not a measurement of a running system." It was
	// written about axis A1, fixed for A1 (nine tg_axis_* series ship today), and falsifiability was never
	// added. Measured 2026-08-07: 9 published tg_axis_* series, none of them this one.
	//
	// SAMPLED SEPARATELY AND FAIL-QUIET, like its sibling: an error here leaves the previous falsifiability
	// sample in place and does NOT fail the whole refresh, because the A1 numbers above are already good.
	if fals, ferr := store.Falsifiability(ctx, now.Add(-s.window)); ferr == nil {
		s.mu.Lock()
		s.fals, s.falsSample = fals, true
		s.mu.Unlock()
	} else {
		log.Printf("axis sampler: falsifiability axis unavailable: %v (the other axes still published)", ferr)
	}

	// Per-dimension judge coverage (TG-360) — SAMPLED SEPARATELY AND FAIL-QUIET, like falsifiability: the
	// two deterministic axes (diagnosis_grounded, estate_grounded) had graded 2 and 1 of 3,371 sessions and
	// nothing on any surface said so, because silence and health looked identical. This publishes judged/
	// eligible per dimension so a silent axis reports itself.
	if cov, judged, cerr := store.JudgmentCoverage(ctx, now.Add(-s.window)); cerr == nil {
		s.mu.Lock()
		s.cov, s.covJudged, s.covSample = cov, judged, true
		s.mu.Unlock()
	} else {
		log.Printf("axis sampler: judgment coverage unavailable: %v (the other axes still published)", cerr)
	}

	// TG-191 (epic TG-187) — the loop-bypass guardrail, sampled for the SAME reason as falsifiability above
	// and stated by the same REQ-2524: its only caller was cmd/axisscore, and "a measurement reachable only
	// by a human running a command is not a measurement of a running system." An executed heal that skipped
	// the prediction->verify loop is drift; a scorecard line nobody runs is not an audit. SAMPLED SEPARATELY
	// AND FAIL-QUIET like its siblings — a failed read here leaves the previous sample and never fails the
	// whole refresh.
	if lb, lerr := store.LoopBypass(ctx, now.Add(-s.window)); lerr == nil {
		s.mu.Lock()
		s.lb, s.lbSample = lb, true
		s.mu.Unlock()
	} else {
		log.Printf("axis sampler: loop-bypass axis unavailable: %v (the other axes still published)", lerr)
	}
	return nil
}

// Collect renders the held sample as /metrics samples. Nothing is emitted until a read has succeeded.
func (s *axisSampler) Collect() []metrics.Sample {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.sampled {
		return nil
	}
	out := make([]metrics.Sample, 0, 4+len(s.agg.DetectionLatency)*3)

	// A1 recall, as a ratio AND as its two components. The ratio alone cannot distinguish "detected 9 of 10"
	// from "detected 900 of 1000", and the denominator is what says whether the number means anything yet.
	out = append(out,
		metrics.Sample{Name: "tg_axis_faults_injected_total", Kind: metrics.Gauge,
			Help:  "Injected faults in the sampling window (axis A1 denominator). 0 ⇒ A1 is not yet measured rather than 0%.",
			Value: float64(s.agg.InjectedFaults)},
		metrics.Sample{Name: "tg_axis_faults_detected_total", Kind: metrics.Gauge,
			Help:  "Injected faults detected inside the detection window, rule-class matched (axis A1 numerator).",
			Value: float64(s.agg.DetectedFaults)},
		metrics.Sample{Name: "tg_axis_sample_age_seconds", Kind: metrics.Gauge,
			Help:  "Seconds since this axis sample was taken. A rising value means the sampler is failing while the numbers below go stale.",
			Value: time.Since(s.at).Seconds()},
	)

	// ★ FALSIFIABILITY (TG-192) — TG's rarest asset, published at last.
	//
	// EVERY FIELD, NOT JUST THE RATE. core/db.Falsifiability carries five counts instead of one for a
	// measured reason recorded there: over 173 live windows a naive `count(*) filter (where falsifiable)`
	// publishes 87% PASS, of which 82% is empty-vs-empty — the real arm found nothing, the shuffled control
	// found nothing, and the row records "the real graph beat its structural control". Restricted to
	// windows where the model actually made a claim it is 61%. Publishing the rate alone would export the
	// 87% artifact, and an exceed-proof is the worst artifact to be wrong in.
	//
	// So the denominator ships beside the numerator and NoClaim ships beside both: a model silent in 129 of
	// 173 windows is itself the finding, and a consumer that only reads the rate can still be corrected by
	// a reader who checks. Gated on its own falsSample flag — before the first successful read there are NO
	// series rather than zeros, because a 0.0 falsifiability rate is the most damaging false statement
	// available about this system.
	if s.falsSample {
		out = append(out,
			metrics.Sample{Name: "tg_axis_falsifiability_windows_total", Kind: metrics.Gauge,
				Help:  "Scored real-vs-shuffled-control windows in the sampling window. The outer denominator.",
				Value: float64(s.fals.Windows)},
			metrics.Sample{Name: "tg_axis_falsifiability_claimed_total", Kind: metrics.Gauge,
				Help: "Windows where the real arm made a claim (RealTP > 0) — the ONLY honest denominator " +
					"for the rate. A window where both arms found nothing passes trivially and means nothing.",
				Value: float64(s.fals.Claimed)},
			metrics.Sample{Name: "tg_axis_falsifiability_noclaim_total", Kind: metrics.Gauge,
				Help: "Windows where the model made NO claim (RealTP == 0), so nothing could vindicate it. " +
					"Measured 82% of naive passes live — carried so the silence is visible rather than " +
					"counted as success.",
				Value: float64(s.fals.NoClaim)},
			metrics.Sample{Name: "tg_axis_falsifiability_claimed_passed_total", Kind: metrics.Gauge,
				Help:  "Windows with a claim where the real graph BEAT its degree-preserving shuffled control.",
				Value: float64(s.fals.ClaimedPassed)},
			metrics.Sample{Name: "tg_axis_falsifiability_passed_naive_total", Kind: metrics.Gauge,
				Help: "Every window marked falsifiable, claim or not — the number a naive count would " +
					"publish. Exported precisely so the overstatement is checkable rather than hidden.",
				Value: float64(s.fals.Passed)},
			metrics.Sample{Name: "tg_axis_falsifiability_real_tp_total", Kind: metrics.Gauge,
				Help:  "True positives of the REAL graph over claimed windows.",
				Value: float64(s.fals.RealTP)},
			metrics.Sample{Name: "tg_axis_falsifiability_control_tp_total", Kind: metrics.Gauge,
				Help: "True positives of the SHUFFLED control over the same windows. The claim is real only " +
					"while this is materially below the real arm; equality means the topology carried nothing.",
				Value: float64(s.fals.ControlTP)},
		)
	}

	// ★ LOOP-BYPASS (TG-191, epic TG-187) — the anti-drift guardrail, published so it is a CONTINUOUS audit
	// and not a line in a command nobody runs (REQ-2524, the same unreachability falsifiability had). The
	// guardrail is Bypassing == 0; a positive value is A5/A3 breadth bought by eroding the falsifiable core,
	// split into its two limbs so a dashboard names WHICH — acted un-predicted, or executed-but-ungraded.
	//
	// THE POPULATION SHIPS BESIDE THE COUNT. tg_axis_loop_bypass_executed_total is the audited denominator:
	// bypassing=0 over executed=0 is "nothing to audit", NOT a clean pass, and a consumer must be able to
	// tell them apart (REQ-2502 applied to this axis). Gated on lbSample — before the first successful read
	// there are NO series rather than a 0 that would read as "audited, no drift" when nothing was audited.
	if s.lbSample {
		out = append(out,
			metrics.Sample{Name: "tg_axis_loop_bypass_executed_total", Kind: metrics.Gauge,
				Help:  "Executed actions in the window — the audited population for the loop-bypass guardrail (TG-191). 0 ⇒ nothing to audit, not a clean pass.",
				Value: float64(s.lb.Executed)},
			metrics.Sample{Name: "tg_axis_loop_bypass_total", Kind: metrics.Gauge,
				Help: "Executed heals that skipped the prediction->verify loop — NO committed prediction OR no " +
					"core/verify grade. The mission guardrail (TG-191, epic TG-187): SHALL be 0; a positive value " +
					"is capability breadth bought by eroding the falsifiable core.",
				Value: float64(s.lb.Bypassing)},
			metrics.Sample{Name: "tg_axis_loop_bypass_no_prediction_total", Kind: metrics.Gauge,
				Help:  "Loop-bypass limb: executed with NO committed infragraph_prediction bound to its action_id (acted un-predicted).",
				Value: float64(s.lb.NoPrediction)},
			metrics.Sample{Name: "tg_axis_loop_bypass_no_verdict_total", Kind: metrics.Gauge,
				Help:  "Loop-bypass limb: executed but core/verify could not grade it (per-execution verdict NULL — TG-182 fail-closed).",
				Value: float64(s.lb.NoVerdict)},
		)
	}

	// ★ TIME TO DECISION (axis A6b, TG-205) — the half of A6 that steps cannot show.
	//
	// A6 is DEFINED as MTTR, and every implementation of it measured decision STEPS: cmd/axisscore's
	// a6a_mean_decision_steps and eval/gate's MeanDecisionSteps. So TG could report how many cycles a decision
	// cost and nothing about how long any single decision took: the only time on this endpoint was
	// tg_agent_run_seconds_total, a cumulative sum with no distribution and no per-incident attribution. Same
	// shape of gap as A1's, where recall could not distinguish a 34-second detector from a 612-second one. Steps are not a proxy for time: the identical
	// two-cycle decision costs seconds on the fast tier and minutes on the reasoning tier, which is precisely
	// the variable the model-tier A/B manipulates.
	//
	// Emitted only when the window holds a session that actually timed its loop. Zero measured sessions must
	// publish NO series rather than a 0 — "TG decides in 0ms" is a flattering falsehood, and the absent-is-not-
	// zero rule that gates the whole sampler applies inside it too.
	if s.agg.DecisionN > 0 {
		out = append(out,
			metrics.Sample{Name: "tg_axis_decision_measured_total", Kind: metrics.Gauge,
				Help:  "Triages in the window whose wall-clock time-to-decision was recorded (axis A6b denominator). Sessions recorded before migration 0058, and suppressed sessions that never ran the loop, are excluded rather than counted as instant.",
				Value: float64(s.agg.DecisionN)},
			metrics.Sample{Name: "tg_axis_decision_latency_p50_seconds", Kind: metrics.Gauge,
				Help:  "Median seconds from composed seed to the terminal decision (proposal or grounded stop) — axis A6b. A6 is defined as MTTR; every other implementation of it measures STEPS, which cannot tell a slow tier from a fast one.",
				Value: float64(s.agg.DecisionMedianMs) / 1000},
			metrics.Sample{Name: "tg_axis_decision_latency_p95_seconds", Kind: metrics.Gauge,
				Help:  "p95 seconds to the terminal decision (axis A6b). Percentiles, never a mean: one gateway stall drags an average arbitrarily.",
				Value: float64(s.agg.DecisionP95Ms) / 1000},
		)
	}

	// ★ PER-DIMENSION JUDGE COVERAGE (TG-360) — so a SILENT axis reports itself.
	//
	// Two deterministic axes (diagnosis_grounded, estate_grounded) were built, calibrated, and rubric-bumped
	// twice, and had graded 2 and 1 of 3,371 sessions — indistinguishable, on every surface, from an axis not
	// plugged in, because the per-dimension MEANS ride the scorecard but judged/eligible was published nowhere.
	//
	// THE DECLARED SET IS EMITTED AT ZERO. A dimension the window did not score must publish scored=0, never
	// vanish: an absent series and a working axis look identical on a dashboard, which is the whole defect. The
	// denominator (sessions judged at all) ships beside it so scored=0 against judged>0 reads as "silent", not
	// "quiet estate" — the sampler's absent-is-not-zero rule applied to the judge's own axes.
	if s.covSample {
		out = append(out, metrics.Sample{Name: "tg_judge_sessions_judged_total", Kind: metrics.Gauge,
			Help:  "Distinct sessions in the window that received ANY judge score — the shared denominator for per-axis coverage. 0 ⇒ the judge produced nothing, not that every axis passed.",
			Value: float64(s.covJudged)})
		byDim := make(map[string]db.DimCoverage, len(s.cov))
		for _, c := range s.cov {
			byDim[c.Dimension] = c
		}
		declared := append([]string{}, judge.Dimensions...)
		declared = append(declared, judge.DimDiagnosisGrounded, judge.DimEstateGrounded)
		for _, dim := range declared {
			c := byDim[dim] // zero value when the axis scored nothing this window
			lbl := map[string]string{"dimension": dim}
			out = append(out, metrics.Sample{Name: "tg_judge_axis_scored_total", Kind: metrics.Gauge,
				Help: "Sessions in the window this judge dimension actually scored (a session_judgment row exists). " +
					"scored=0 against a non-zero tg_judge_sessions_judged_total means the axis is SILENT — plugged " +
					"in but grading nothing — which reads identically to a healthy axis unless published (TG-360).",
				Value: float64(c.Scored), Labels: lbl})
			if c.Scored > 0 {
				out = append(out, metrics.Sample{Name: "tg_judge_axis_mean", Kind: metrics.Gauge,
					Help:  "Mean score for this dimension over the sessions it scored. Emitted only when the axis scored ≥1 session — a mean over zero rows is not a measurement.",
					Value: c.Mean, Labels: lbl})
			}
		}
	}

	// ★ THE PER-SOURCE LATENCY DISTRIBUTION — the half of A1 that recall cannot show. `detections` is
	// FIRST-reports only: once a fault is detected, a slower source reporting it later has detected nothing
	// new, so these counts partition the detected faults across sources rather than double-counting them.
	for _, l := range s.agg.DetectionLatency {
		lbl := map[string]string{"source": l.Source}
		out = append(out,
			metrics.Sample{Name: "tg_axis_detection_first_reports_total", Kind: metrics.Gauge,
				Help:  "Faults this ingest source reported FIRST in the window. Partitions the detected set across sources — a slower source reporting a known fault later is not counted.",
				Value: float64(l.Detections), Labels: lbl},
			metrics.Sample{Name: "tg_axis_detection_latency_p50_seconds", Kind: metrics.Gauge,
				Help:  "Median seconds from fault injection to this source's first alert. This is the axis a faster detector actually moves; A1 recall is nearly insensitive to it.",
				Value: float64(l.MedianSec), Labels: lbl},
			metrics.Sample{Name: "tg_axis_detection_latency_p95_seconds", Kind: metrics.Gauge,
				Help:  "p95 seconds from fault injection to this source's first alert.",
				Value: float64(l.P95Sec), Labels: lbl},
		)
	}
	return out
}

// startAxisSampler arms the periodic sample. interval <= 0 leaves it unarmed (and therefore silent), which is
// the correct posture for an install with no injected faults to measure.
func startAxisSampler(ctx context.Context, s *axisSampler, store *db.AxisReadStore, interval time.Duration,
	logf func(string, ...any)) {
	if s == nil || store == nil || interval <= 0 {
		return
	}
	run := func() {
		c, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		if err := s.sample(c, store, time.Now().UTC()); err != nil {
			logf("axis sampler: read failed: %v (keeping the previous sample; no series is invented)", err)
		}
	}
	run() // one sample immediately, so /metrics is populated without waiting a full interval
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				run()
			}
		}
	}()
}
