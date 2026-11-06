package skillstore

import (
	"math"
	"testing"
)

// The golden values are the PREDECESSOR Python implementation's outputs (prompt_patch_trial.py
// welch_one_sided, generated 2026-07-19) — the Go port must match digit-for-digit so three months of
// production-tuned statistical behavior carries over unchanged (REQ-1308).
func TestWelchOneSidedGoldenValues(t *testing.T) {
	cases := []struct {
		name  string
		a, b  []float64
		wantT float64
		wantP float64
	}{
		{"clear lift K=15", []float64{4.1, 4.3, 3.9, 4.5, 4.2, 4.0, 4.4, 4.1, 4.3, 4.2, 4.0, 4.4, 3.9, 4.5, 4.1},
			[]float64{3.5, 3.6, 3.4, 3.8, 3.5, 3.7, 3.3, 3.6, 3.5, 3.4, 3.7, 3.6, 3.5, 3.4, 3.6},
			10.4214889215, 0.0000000001},
		{"identical arms", []float64{3.0, 3.1, 2.9}, []float64{3.0, 3.1, 2.9}, 0.0, 0.5},
		{"zero variance both arms", []float64{4.0, 4.0, 4.0, 4.0}, []float64{3.0, 3.0, 3.0, 3.0}, 0.0, 0.5},
		{"noise no lift", []float64{3.2, 3.8, 2.9, 4.1, 3.5}, []float64{3.4, 3.6, 3.1, 3.9, 3.3},
			0.1586103171, 0.4392836813},
		{"high variance same mean", []float64{1.0, 5.0, 1.0, 5.0, 1.0, 5.0}, []float64{3.0, 3.0, 3.0, 3.0, 3.0, 3.0}, 0.0, 0.5},
		{"too few samples", []float64{4.2}, []float64{3.1, 3.3}, 0.0, 1.0},
	}
	for _, c := range cases {
		gotT, gotP := WelchOneSided(c.a, c.b)
		if math.Abs(gotT-c.wantT) > 1e-9 {
			t.Errorf("%s: t = %.10f, want %.10f", c.name, gotT, c.wantT)
		}
		if math.Abs(gotP-c.wantP) > 1e-9 {
			t.Errorf("%s: p = %.10f, want %.10f", c.name, gotP, c.wantP)
		}
	}
}

// The test is properly ONE-sided: a candidate WORSE than control must never look significant.
func TestWelchOneSidedWorseCandidateNeverSignificant(t *testing.T) {
	worse := []float64{2.1, 2.3, 1.9, 2.5, 2.2, 2.0, 2.4, 2.1, 2.3, 2.2, 2.0, 2.4, 1.9, 2.5, 2.1}
	control := []float64{3.5, 3.6, 3.4, 3.8, 3.5, 3.7, 3.3, 3.6, 3.5, 3.4, 3.7, 3.6, 3.5, 3.4, 3.6}
	tt, p := WelchOneSided(worse, control)
	if tt >= 0 || p < 0.99 {
		t.Fatalf("a worse candidate must have negative t and p near 1, got t=%.4f p=%.4f", tt, p)
	}
}
