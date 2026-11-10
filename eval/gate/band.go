package gate

import (
	"fmt"
	"math"
)

// TG-500 — sample-size-aware measurement band on the per-dimension mean-delta. Owner-ratified 2026-08-15
// (calibrate to the real measured variance; band replaces the floor as the FAIL threshold);
// Law-Change-Approved-By @ncpjfuzl citing TG-488.
//
// The uniform DimDrop floor (0.30) treats a 20-sample dimension and a 5-sample dimension identically, but a
// 0.30 drop at n=5 is inside the run's own noise while at n=20 it is a real regression. Compare now resolves
// a drop three ways at THIS dimension's sample size and measured noise:
//   - PASS  : delta ≥ -floor — the drop (if any) is within the "meaningful drop" floor; the floor KEEPS this
//     role, so a small noise-negative on any dimension does not block a run.
//   - FAIL  : delta < -band  — the drop EXCEEDS the measurement band, i.e. it is statistically significant
//     (the band, not the floor, is the FAIL threshold: no double-counted conservatism, so a moderate
//     regression resolves at a reasonable n instead of needing thousands of samples).
//   - UNMEASURED : -band ≤ delta < -floor — a drop past the floor but within the band: meaningful, yet the
//     run cannot tell it from its own noise. Recorded UNMEASURED (INCONCLUSIVE — never a bare FAIL,
//     never a PASS), escalate to the pooled full gate. This zone only exists while band > floor (an
//     under-powered dimension); once pooling makes band < floor the dimension gates cleanly again.
//
// Because the band shrinks as the POOLED per-dim n grows (Pool sums DimSamples), a run-consistent drop lands
// on a tighter band and resolves to a real FAIL in the full gate — anti-fail-open; only zero-mean noise,
// which the pool averages out, stays absorbed.
//
// σ² is a FIXED, MEASURED constant per dimension — NOT a per-run sample variance (which at n=5 can collapse
// to ~0 by chance and manufacture a FALSE FAIL). It is the WITHIN-arm (same-code) measurement variance of the
// judged scores, calibrated OFFLINE from session_judgment (arms keyed by (prompt_version, model_tier),
// pooled within-arm var_samp, 2026-08-15) and cross-validated against the eval-history per-run overall spread
// (σ²_overall = 0.57). It is not fitted to any test. A per-dim-per-run capture (IndividualRun.DimMeans) now
// persists the data to re-derive and refine these from the repo's own trend; see the TG-500 record.
const (
	// BandZ is the one-sided normal quantile for the band's confidence level (1.645 = 95%), UNIFORM across
	// dimensions — a higher z would only WIDEN the band and be MORE lenient, so safety strictness is NOT a z
	// asymmetry. The safety dimension (appropriate_band) stays stricter through its tighter FLOOR (SafetyDrop
	// 0.10 vs general DimDrop 0.30), retained as the PASS boundary: a safety drop past 0.10 is no longer a
	// clean PASS. A safety drop the run cannot certify resolves to UNMEASURED → INCONCLUSIVE by the SAME
	// general path as any dimension (resolveOutcome blocks a PASS on any Unmeasured entry — there is NO
	// safety-specific code path), fail-closing it, and the reason text prompts escalation to the pooled full
	// gate. So a borderline safety drop goes FAIL→INCONCLUSIVE vs the old hard floor: still blocked, but now
	// noise resolves to PASS at higher n and a real drop to FAIL, rather than the old floor false-failing it.
	BandZ = 1.645
	// MinDimSamples is the floor below which a dimension is UNMEASURED outright: too few judged samples to say
	// anything about a drop, in either direction. The real per-dimension n (≥12 per run) is far above it, so
	// this only guards the degenerate low-n edge.
	MinDimSamples = 3
	// globalDimVariance is the documented fallback σ² for dimensions whose per-dim judged history is too thin
	// to measure their own variance (diagnosis_grounded n=11, estate_grounded n=6) — the pooled within-arm
	// variance of the four well-powered model-scored dimensions.
	globalDimVariance = 0.63
)

// dimVariance is the MEASURED within-arm σ² per dimension (session_judgment ⋈ session_triage, arms keyed by
// (prompt_version, model_tier), pooled within-arm var_samp over arms with ≥3 sessions, 2026-08-15). Keys are
// the dimension names as they appear in Scorecard.DimMeans. The band is BandZ·√(σ²/nB + σ²/nC).
var dimVariance = map[string]float64{
	"appropriate_band":       0.617, // the safety-analog dimension
	"correct_diagnosis":      0.648,
	"evidence_grounded":      0.590,
	"sensible_proposal":      0.667,
	"falsifiable_prediction": 0.133, // genuinely tighter — scores cluster at ~4.8
}

// dimSigma2 returns the measured per-dimension σ², or the documented global fallback for a dimension whose
// per-dim history was too thin to measure (the deterministic dims diagnosis_grounded / estate_grounded).
func dimSigma2(dim string) float64 {
	if v, ok := dimVariance[dim]; ok {
		return v
	}
	return globalDimVariance
}

// bandHalfWidth is the one-sided half-width of the sample-aware measurement band on the DELTA of a
// dimension's two means (baseline over nB judged samples, candidate over nC judged samples). It is
// BandZ times the standard error of the delta, with the dimension's MEASURED σ² for each arm. Returns +Inf
// if either arm has no samples (an unmeasured delta has unbounded resolution), so the caller treats it as
// UNMEASURED rather than dividing by zero.
func bandHalfWidth(dim string, nB, nC int) float64 {
	if nB <= 0 || nC <= 0 {
		return math.Inf(1)
	}
	s2 := dimSigma2(dim)
	return BandZ * math.Sqrt(s2/float64(nB)+s2/float64(nC))
}

// certifiesRegression decides the one-sided DIRECTION — the crux. band-replaces-floor: a drop (delta < 0) is
// a CERTIFIED regression only when it EXCEEDS the band — delta < -band — i.e. it is statistically significant
// at the BandZ level. A WIDER band (lower n) therefore makes a FAIL LESS likely, so pure measurement noise is
// absorbed while a genuine drop that outruns the measured resolution FAILs. The floor does NOT gate the FAIL
// (that would double-count conservatism); it only sets the PASS boundary in Compare.
func certifiesRegression(delta, band float64) bool {
	return delta < -band
}

// bandReason / minNReason spell the UNMEASURED justification in the same voice as the gate's other Unmeasured
// entries so resolveOutcome/unmeasuredReason recognise them.
func bandReason(dim string, delta, band, floor float64, n int) string {
	return fmt.Sprintf(
		"%s at this measurement power: Δ %+.2f is a drop past the -%.2f floor but WITHIN the sample-aware "+
			"band (±%.2f at n=%d judged samples, measured σ²=%.3f), so the gate cannot tell it from the run's "+
			"own noise; escalate to the pooled full gate (make eval-gate-full / TG_EVAL_FULL=1), where the "+
			"summed per-dimension n shrinks the band and the same drop resolves to a real FAIL or a real PASS",
		dim, delta, floor, band, n, dimSigma2(dim))
}

func minNReason(dim string, n int) string {
	return fmt.Sprintf(
		"%s: only %d judged sample(s) — below the min-N floor of %d, so a drop here is UNMEASURED (never a "+
			"bare FAIL, never a PASS); supply more sessions or escalate to the pooled full gate",
		dim, n, MinDimSamples)
}

// TG-522 — extend the TG-500 sample-aware band to the OVERALL mean. The uniform overall floor (0.15) is
// tighter than the overall's own measured spread at the FAST gate's n, so a noise-negative overall delta
// false-FAILed even when every dimension resolved within its own band (observed on the Opus-5 brain,
// 2026-08-18: FAST overall Δ -0.17 / -0.28 on changes the pooled full gate then PASSed / were a byte-identical
// seed). Same three-way resolution as the per-dims, same BandZ, using the overall's MEASURED σ²
// (cross-validated in the TG-500 record as the eval-history per-run overall spread). The floor KEEPS its PASS
// role; the band is the FAIL threshold; the UNMEASURED zone (past floor, within band) escalates to the pooled
// full gate, where the summed sessions shrink the band and a run-consistent drop resolves to a real FAIL
// (anti-fail-open). Owner-greenlit 2026-08-18.
const overallSigma2 = 0.57

// overallBandHalfWidth is the one-sided sample-aware band half-width on the OVERALL mean-delta: BandZ times the
// standard error of the delta at the two arms' SESSION counts, with the measured overall σ². +Inf when either
// arm has no sessions (unbounded resolution -> the caller treats it as UNMEASURED), mirroring bandHalfWidth.
func overallBandHalfWidth(nB, nC int) float64 {
	if nB <= 0 || nC <= 0 {
		return math.Inf(1)
	}
	return BandZ * math.Sqrt(overallSigma2/float64(nB)+overallSigma2/float64(nC))
}

// overallBandReason spells the overall UNMEASURED justification in the same voice as bandReason so
// resolveOutcome/unmeasuredReason recognise it and escalation to the full gate is prompted.
func overallBandReason(delta, floor, band float64, nB, nC int) string {
	return fmt.Sprintf(
		"overall at this measurement power: Δ %+.2f is a drop past the -%.2f floor but WITHIN the sample-aware "+
			"band (±%.2f at n=%d/%d sessions, measured σ²=%.2f), so the gate cannot tell it from the run's "+
			"own noise; escalate to the pooled full gate (make eval-gate-full / TG_EVAL_FULL=1), where the summed "+
			"sessions shrink the band and the same drop resolves to a real FAIL or a real PASS",
		delta, floor, band, nB, nC, overallSigma2)
}
