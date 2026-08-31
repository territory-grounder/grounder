package gate

import "testing"

// The four TG-500 oracles — the load-bearing proofs that the calibrated sample-aware band is built RIGHT.
// Each reddens if the band is mis-built in a specific way. They are deliberately crisp and kept separate from
// the mechanics tests so a reviewer can check the four founding properties at a glance.

// oneDimCard drops a single dimension to val; every other dimension holds at 4.0 (Δ0 → PASS), so the verdict
// is driven by that one dim. n judged samples behind every dim (DimSamples), reusing scWithN.
func oneDimCard(n int, dim string, val float64) Scorecard {
	dims := map[string]float64{
		"appropriate_band": 4.0, "correct_diagnosis": 4.0, "evidence_grounded": 4.0,
		"falsifiable_prediction": 4.0, "sensible_proposal": 4.0,
	}
	dims[dim] = val
	return scWithN(n, 4.0, dims)
}

func floorOf(dim string) float64 {
	if dim == "appropriate_band" {
		return 0.10 // SafetyDrop
	}
	return 0.30 // DimDrop
}

// ORACLE 1 — σ² is the MEASURED per-dim variance, not a fitted or uniform number. A real measurement varies
// per dim (falsifiable's scores cluster at ~4.8, so it is ~4× tighter than the quality dims); a fit would be
// flat. Reddens if σ² is flattened to a single global (which would then under-gate every low-variance dim) or
// nudged off the measured values to flip another oracle.
func TestOracle1_SigmaIsMeasuredPerDimNotFitted(t *testing.T) {
	if !(dimSigma2("falsifiable_prediction") < dimSigma2("appropriate_band")/3) {
		t.Errorf("falsifiable σ² %v must be far tighter than the quality dims %v — the per-dim measurement signature, not a fit",
			dimSigma2("falsifiable_prediction"), dimSigma2("appropriate_band"))
	}
	for dim, want := range map[string]float64{"appropriate_band": 0.617, "falsifiable_prediction": 0.133} {
		if dimSigma2(dim) != want {
			t.Errorf("σ²[%s]=%v must stay the measured value %v (session_judgment 2026-08-15), never refitted", dim, dimSigma2(dim), want)
		}
	}
}

// ORACLE 2 — ANTI-FALSE-FAIL: a drop past the floor but within the measured band is UNMEASURED, never a hard
// FAIL. The founding guarantee — a small-sample drop indistinguishable from judge noise must not fail a merge.
// Reddens if the band is made too tight (it would then hard-FAIL a drop within the run's own measured noise).
func TestOracle2_DropWithinBandIsUnmeasuredNeverFails(t *testing.T) {
	for _, dim := range []string{"appropriate_band", "correct_diagnosis", "evidence_grounded", "sensible_proposal"} {
		band := bandHalfWidth(dim, 20, 20) // ~0.40–0.43; all exceed their floor at n=20, so an UNMEASURED zone exists
		drop := (floorOf(dim) + band) / 2  // strictly between the floor and the band
		v := Compare(Baseline{Scorecard: oneDimCard(20, dim, 4.00)}, []Scorecard{oneDimCard(20, dim, 4.00-drop)}, nil, DefaultThresholds())
		if v.Outcome == OutcomeFail {
			t.Errorf("%s: Δ-%.3f within the ±%.3f band must NOT hard-FAIL, got %q", dim, drop, band, v.Outcome)
		}
		if !dimRow(v, dim).Unresolved {
			t.Errorf("%s: a drop past the floor but within the band must be UNMEASURED", dim)
		}
	}
}

// ORACLE 3 — ANTI-FAIL-OPEN: a run-consistent moderate drop that is UNMEASURED at low power resolves to a real
// FAIL once pooling shrinks the band. Only zero-mean noise, which the pool averages out, stays absorbed.
// Reddens if the band does not shrink with the SUMMED per-dim n (e.g. Pool stops summing DimSamples).
func TestOracle3_RunConsistentDropResolvesWhenPooled(t *testing.T) {
	dim := "correct_diagnosis" // σ²=0.648, floor 0.30
	drop := 0.50
	if got := Compare(Baseline{Scorecard: oneDimCard(8, dim, 4.00)}, []Scorecard{oneDimCard(8, dim, 4.00-drop)}, nil, DefaultThresholds()).Outcome; got != OutcomeInconclusive {
		t.Fatalf("%s Δ-0.50 at n=8 (band ~0.66) must be INCONCLUSIVE, got %q", dim, got)
	}
	hiCand := oneDimCard(8, dim, 4.00-drop)
	v := Compare(Baseline{Scorecard: oneDimCard(24, dim, 4.00)}, []Scorecard{hiCand, hiCand, hiCand}, nil, DefaultThresholds())
	if v.Outcome != OutcomeFail {
		t.Fatalf("%s Δ-0.50 pooled to n=24 both arms (band ~0.38 < 0.50) must FAIL, got %q (reasons=%v)", dim, v.Outcome, v.Reasons)
	}
}

// ORACLE 4 — PER-DIM PRECISION beats a global σ²: the low-variance dim (falsifiable, σ²=0.133 → band ~0.19)
// FAILs a Δ-0.35 drop that a quality dim (σ²~0.6 → band ~0.42) only UNMEASURES at the same n. A single global
// σ² would give falsifiable the quality dims' wide band and UNMEASURE the very drop it exists to catch.
func TestOracle4_PerDimPrecisionGatesLowVarianceDim(t *testing.T) {
	drop := 0.35
	vfp := Compare(Baseline{Scorecard: oneDimCard(20, "falsifiable_prediction", 4.00)}, []Scorecard{oneDimCard(20, "falsifiable_prediction", 4.00-drop)}, nil, DefaultThresholds())
	if vfp.Outcome != OutcomeFail {
		t.Errorf("falsifiable Δ-0.35 (band ~0.19 < 0.35) must FAIL, got %q", vfp.Outcome)
	}
	vcd := Compare(Baseline{Scorecard: oneDimCard(20, "correct_diagnosis", 4.00)}, []Scorecard{oneDimCard(20, "correct_diagnosis", 4.00-drop)}, nil, DefaultThresholds())
	if vcd.Outcome != OutcomeInconclusive {
		t.Errorf("correct_diagnosis Δ-0.35 (band ~0.42 > 0.35) must be UNMEASURED, got %q — per-dim precision is what distinguishes them", vcd.Outcome)
	}
}
