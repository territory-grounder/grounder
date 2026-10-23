package calibrate

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// An empty sample set is an honest "no evidence yet": zero scores, empty-count bins — never a fabricated curve.
func TestComputeEmpty(t *testing.T) {
	r := Compute(nil, 10)
	if r.N != 0 || r.Brier != 0 || r.ECE != 0 || r.MCE != 0 || len(r.Bins) != 10 {
		t.Fatalf("empty must be zero-value with 10 bins, got %+v", r)
	}
	if r.Bins[9].Hi != 1.0 || r.Bins[0].Lo != 0.0 {
		t.Fatalf("bin bounds wrong: %+v", r.Bins)
	}
}

// Perfectly calibrated: a 0.8-confidence cohort that resolves clean 80% of the time has ECE 0.
func TestComputePerfectlyCalibrated(t *testing.T) {
	var s []Sample
	for i := 0; i < 10; i++ {
		s = append(s, Sample{Confidence: 0.8, Clean: i < 8}) // 8 of 10 clean
	}
	r := Compute(s, 10)
	if r.N != 10 {
		t.Fatalf("N=%d", r.N)
	}
	if !approx(r.ECE, 0) {
		t.Fatalf("perfectly-calibrated ECE must be 0, got %v", r.ECE)
	}
	// Brier = [8*(0.8-1)^2 + 2*(0.8-0)^2]/10 = [8*0.04 + 2*0.64]/10 = 1.6/10 = 0.16
	if !approx(r.Brier, 0.16) {
		t.Fatalf("Brier = %v, want 0.16", r.Brier)
	}
}

// Overconfident: a 0.9-confidence cohort that only resolves clean 50% of the time has ECE/MCE = 0.4, and the
// gap is positive (overconfident) — the expected RLHF direction the design defends against.
func TestComputeOverconfident(t *testing.T) {
	var s []Sample
	for i := 0; i < 100; i++ {
		s = append(s, Sample{Confidence: 0.9, Clean: i < 50})
	}
	r := Compute(s, 10)
	if !approx(r.ECE, 0.4) {
		t.Fatalf("overconfident ECE = %v, want 0.4", r.ECE)
	}
	if !approx(r.MCE, 0.4) {
		t.Fatalf("MCE = %v, want 0.4", r.MCE)
	}
	top := r.Bins[9] // conf 0.9 → top bin [0.9,1.0]
	if top.Count != 100 || !approx(top.MeanConf, 0.9) || !approx(top.CleanRate, 0.5) {
		t.Fatalf("top bin = %+v", top)
	}
	if top.MeanConf-top.CleanRate <= 0 {
		t.Fatalf("gap must be positive (overconfident): %+v", top)
	}
}

// Confidence 1.0 with a clean outcome is perfect: Brier 0, ECE 0, and 1.0 lands in the top bin (inclusive).
func TestComputePerfectConfidence(t *testing.T) {
	r := Compute([]Sample{{Confidence: 1.0, Clean: true}, {Confidence: 1.0, Clean: true}}, 10)
	if !approx(r.Brier, 0) || !approx(r.ECE, 0) {
		t.Fatalf("perfect: Brier=%v ECE=%v", r.Brier, r.ECE)
	}
	if r.Bins[9].Count != 2 {
		t.Fatalf("conf 1.0 must land in the top bin, got %+v", r.Bins)
	}
}

// Out-of-range confidence is clamped defensively (the proposal grammar already bounds [0,1], but a store bug
// must not panic or mis-bin): 1.5 → top bin, -0.2 → bottom bin.
func TestComputeClampsOutOfRange(t *testing.T) {
	r := Compute([]Sample{{Confidence: 1.5, Clean: true}, {Confidence: -0.2, Clean: false}}, 10)
	if r.Bins[9].Count != 1 || r.Bins[0].Count != 1 {
		t.Fatalf("clamp failed: %+v", r.Bins)
	}
	if r.N != 2 {
		t.Fatalf("N=%d, want 2", r.N)
	}
}

// bins < 1 is treated as a single bin (never a divide-by-zero / empty-slice panic).
func TestComputeDegenerateBins(t *testing.T) {
	r := Compute([]Sample{{Confidence: 0.5, Clean: true}}, 0)
	if len(r.Bins) != 1 || r.Bins[0].Count != 1 {
		t.Fatalf("bins<1 must collapse to one bin, got %+v", r.Bins)
	}
}
