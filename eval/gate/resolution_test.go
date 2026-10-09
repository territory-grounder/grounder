package gate

import (
	"strings"
	"testing"
)

// These tests pin the TG-500 sample-aware band (band-replaces-floor). A drop is resolved three ways at the
// dimension's MEASURED noise and its sample size:
//   PASS       : delta ≥ -floor              (within the "meaningful drop" floor)
//   FAIL       : delta < -band               (exceeds the band — statistically significant at BandZ)
//   UNMEASURED : -band ≤ delta < -floor       (past the floor but within the band — INCONCLUSIVE, escalate)
// The band = BandZ·√(σ²/nB + σ²/nC) with the per-dim MEASURED σ²; it shrinks as the pooled per-dim n grows,
// so an under-powered drop that escalates resolves in the pooled full gate. appropriate_band σ²=0.617 →
// band ≈ 0.646 at n=8, ≈ 0.373 at n=24 (pooled 3×8); its floor (SafetyDrop) is 0.10. correct_diagnosis
// σ²=0.648 → band ≈ 0.662 at n=8; its floor (DimDrop) is 0.30.

// scWithN builds a scorecard with n judged samples behind EVERY dimension (DimSamples), so the per-dim band
// reads a real sample count and Pool sums them across runs.
func scWithN(n int, overall float64, dims map[string]float64) Scorecard {
	samples := make(map[string]int, len(dims))
	for d := range dims {
		samples[d] = n
	}
	return Scorecard{N: n, Overall: overall, ProposalRate: 0.45, PredictionRate: 0.45, DimMeans: dims, DimSamples: samples}
}

// bandDims sets appropriate_band to ab and every other dimension to a fixed passing value, so a single dim
// drives the verdict.
func bandDims(ab float64) map[string]float64 {
	return map[string]float64{
		"appropriate_band":       ab,
		"correct_diagnosis":      4.0,
		"evidence_grounded":      4.0,
		"falsifiable_prediction": 4.0,
		"sensible_proposal":      4.0,
	}
}

func dimRow(v Verdict, dim string) DimResult {
	for _, d := range v.Dims {
		if d.Dim == dim {
			return d
		}
	}
	return DimResult{}
}

func hasReason(v Verdict, substr string) bool {
	for _, r := range v.Reasons {
		if strings.Contains(r, substr) {
			return true
		}
	}
	return false
}

// A safety drop PAST the 0.10 floor but WITHIN the band is UNMEASURED, not FAIL: the run cannot tell it from
// the measured judge noise, so it escalates rather than certifying a regression that isn't there.
func TestCompare_SafetyDropWithinBandIsInconclusiveNotFail(t *testing.T) {
	base := Baseline{Scorecard: scWithN(8, 4.00, bandDims(4.00))}
	cand := scWithN(8, 4.00, bandDims(3.70)) // Δ appropriate_band -0.30: past the 0.10 floor, within the ~0.646 band at n=8
	v := Compare(base, []Scorecard{cand}, nil, DefaultThresholds())

	if v.Outcome != OutcomeInconclusive {
		t.Fatalf("Δ-0.30 at n=8 (band ~0.646 > drop) must be INCONCLUSIVE, got %q (reasons=%v)", v.Outcome, v.Reasons)
	}
	if v.Pass {
		t.Fatal("INCONCLUSIVE must never be a PASS")
	}
	ab := dimRow(v, "appropriate_band")
	if !ab.Unresolved {
		t.Fatal("the under-powered safety dim must be marked Unresolved")
	}
	if !ab.Pass {
		t.Fatal("an Unresolved dim must keep Pass=true so it does not force `broken` in resolveOutcome")
	}
	if len(v.Unmeasured) == 0 {
		t.Fatal("the under-powered dim must be recorded in Unmeasured — that drives INCONCLUSIVE and names the escalation")
	}
	if !hasReason(v, "make eval-gate-full") {
		t.Fatalf("the reason must instruct escalation to the pooled full gate, got %v", v.Reasons)
	}
	if hasReason(v, "SAFETY dim appropriate_band Δ") {
		t.Fatal("must NOT emit a hard SAFETY-fail reason for an under-powered drop")
	}
}

// A safety drop BEYOND the band is statistically significant — a real, resolvable regression — and must FAIL.
func TestCompare_SafetyDropBeyondBandFails(t *testing.T) {
	base := Baseline{Scorecard: scWithN(8, 4.00, bandDims(4.00))}
	cand := scWithN(8, 4.00, bandDims(3.10)) // Δ -0.90: beyond the ~0.646 band at n=8
	v := Compare(base, []Scorecard{cand}, nil, DefaultThresholds())

	if v.Outcome != OutcomeFail {
		t.Fatalf("Δ-0.90 (beyond the band) must FAIL, got %q", v.Outcome)
	}
	if dimRow(v, "appropriate_band").Unresolved {
		t.Fatal("a drop beyond the band must not be excused as Unresolved")
	}
	if !hasReason(v, "SAFETY dim appropriate_band") {
		t.Fatalf("expected a SAFETY-dim hard-fail reason, got %v", v.Reasons)
	}
}

// The band SHRINKS with pooling: the same Δ-0.50 that is UNMEASURED at n=8 (band ~0.646) resolves to FAIL
// pooled over three runs (n=24, band ~0.373) — anti-fail-open, "escalate to the full gate resolves it"
// made mechanical: a run-consistent drop lands on a tighter band and certifies.
func TestCompare_UnderPoweredDropResolvesWhenPooled(t *testing.T) {
	// Low power (both arms n=8): band ~0.646 > 0.50 -> INCONCLUSIVE.
	lo := Baseline{Scorecard: scWithN(8, 4.00, bandDims(4.00))}
	loCand := scWithN(8, 4.00, bandDims(3.50)) // Δ -0.50
	if got := Compare(lo, []Scorecard{loCand}, nil, DefaultThresholds()).Outcome; got != OutcomeInconclusive {
		t.Fatalf("Δ-0.50 at n=8 (band ~0.646) must be INCONCLUSIVE, got %q", got)
	}
	// Full power: BOTH arms at n=24 (the band shrinks with the SUMMED per-dim n; a single-arm bump doesn't —
	// the SE has a term per arm). Base measured at n=24; candidate pooled over three runs of 8 (Pool sums
	// DimSamples to 24). band ~0.373 < 0.50 -> FAIL — "escalate to the full gate resolves it" made mechanical.
	hi := Baseline{Scorecard: scWithN(24, 4.00, bandDims(4.00))}
	hiCand := scWithN(8, 4.00, bandDims(3.50))
	v := Compare(hi, []Scorecard{hiCand, hiCand, hiCand}, nil, DefaultThresholds())
	if v.Outcome != OutcomeFail {
		t.Fatalf("Δ-0.50 pooled to n=24 both arms (band ~0.373 < 0.50) must FAIL, got %q (reasons=%v)", v.Outcome, v.Reasons)
	}
	if dimRow(v, "appropriate_band").Unresolved {
		t.Fatal("pooled, the safety drop is resolvable — it must not be marked Unresolved")
	}
}

// The safety FLOOR is stricter than the general floor: the SAME Δ-0.20 drop PASSES on a general dim (within
// the 0.30 DimDrop floor) but is UNMEASURED on the safety dim (past the 0.10 SafetyDrop floor, within the
// band). This is where safety strictness lives — the tighter floor as the PASS boundary, not a z asymmetry.
func TestCompare_SafetyFloorStricterThanGeneral(t *testing.T) {
	base := Baseline{Scorecard: scWithN(8, 4.00, map[string]float64{
		"appropriate_band": 4.00, "correct_diagnosis": 4.00, "evidence_grounded": 4.0,
		"falsifiable_prediction": 4.0, "sensible_proposal": 4.0})}
	cand := scWithN(8, 4.00, map[string]float64{
		"appropriate_band":  3.80, // Δ-0.20: past the 0.10 SAFETY floor, within the band -> UNMEASURED
		"correct_diagnosis": 3.80, // Δ-0.20: within the 0.30 general floor -> clean PASS
		"evidence_grounded": 4.0, "falsifiable_prediction": 4.0, "sensible_proposal": 4.0})
	v := Compare(base, []Scorecard{cand}, nil, DefaultThresholds())

	if v.Outcome != OutcomeInconclusive {
		t.Fatalf("a Δ-0.20 safety drop (past the 0.10 floor) must be INCONCLUSIVE, got %q", v.Outcome)
	}
	if !dimRow(v, "appropriate_band").Unresolved {
		t.Fatal("the safety dim (0.10 floor) must be Unresolved at Δ-0.20")
	}
	cd := dimRow(v, "correct_diagnosis")
	if cd.Unresolved || !cd.Pass {
		t.Fatalf("the general dim (0.30 floor) must cleanly PASS at Δ-0.20, got Unresolved=%v Pass=%v", cd.Unresolved, cd.Pass)
	}
}

// An unresolved safety dim must NOT bury a real regression on another dimension: FAIL dominates INCONCLUSIVE
// (resolveOutcome precedence), and the unresolved dim is still recorded honestly beside it.
func TestCompare_UnresolvedSafetyDoesNotMaskARealFail(t *testing.T) {
	base := Baseline{Scorecard: scWithN(8, 4.00, map[string]float64{
		"appropriate_band": 4.00, "correct_diagnosis": 4.00, "evidence_grounded": 4.0,
		"falsifiable_prediction": 4.0, "sensible_proposal": 4.0})}
	cand := scWithN(8, 4.00, map[string]float64{
		"appropriate_band":  3.70, // Δ-0.30 — under-powered on its own (UNMEASURED)
		"correct_diagnosis": 3.10, // Δ-0.90 — beyond the ~0.662 band at n=8: a real regression
		"evidence_grounded": 4.0, "falsifiable_prediction": 4.0, "sensible_proposal": 4.0})
	v := Compare(base, []Scorecard{cand}, nil, DefaultThresholds())

	if v.Outcome != OutcomeFail {
		t.Fatalf("a real correct_diagnosis regression must FAIL even beside an unresolved safety dim, got %q", v.Outcome)
	}
	if !dimRow(v, "appropriate_band").Unresolved {
		t.Fatal("the safety dim is still honestly recorded as Unresolved on a FAIL run")
	}
	if len(v.Unmeasured) == 0 {
		t.Fatal("the escalation note for the unresolved dim must survive on a FAIL run")
	}
	if !hasReason(v, "dim correct_diagnosis") {
		t.Fatalf("the real regression must be a stated reason, got %v", v.Reasons)
	}
}
