package policy

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// f64 and intp pointer helpers are shared from rule_test.go.

// TestClampConfidence_BelowThreshold — an auto below min_confidence clamps to approve (REQ-1507).
func TestClampConfidence_BelowThreshold(t *testing.T) {
	v, rec := ClampConfidence(VerdictAuto, 0.30, 0.60)
	if v != VerdictApprove {
		t.Fatalf("below-threshold auto = %q, want approve", v)
	}
	if !rec.Clamped || rec.VerdictIn != VerdictAuto || rec.VerdictOut != VerdictApprove {
		t.Fatalf("record did not capture the clamp: %+v", rec)
	}
}

// TestClampConfidence_AtOrAbove — an auto at/above min_confidence is unchanged (REQ-1507).
func TestClampConfidence_AtOrAbove(t *testing.T) {
	for _, c := range []float64{0.60, 0.75, 1.0} {
		if v, rec := ClampConfidence(VerdictAuto, c, 0.60); v != VerdictAuto || rec.Clamped {
			t.Fatalf("confidence %.2f: verdict %q clamped=%v, want auto retained", c, v, rec.Clamped)
		}
	}
}

// TestClampConfidence_ApproveDenyUnaffected — the confidence gate never touches approve/deny (only auto).
func TestClampConfidence_ApproveDenyUnaffected(t *testing.T) {
	for _, in := range []Verdict{VerdictApprove, VerdictDeny} {
		// Even a wildly below-threshold confidence must not change a non-auto verdict.
		if v, rec := ClampConfidence(in, 0.01, 0.99); v != in || rec.Clamped {
			t.Fatalf("verdict %q was altered by the confidence gate: got %q clamped=%v", in, v, rec.Clamped)
		}
	}
}

// TestClampConfidence_FailClosed — NaN / out-of-range confidence clamps auto→approve (INV-09 fail-closed).
func TestClampConfidence_FailClosed(t *testing.T) {
	for _, c := range []float64{math.NaN(), -0.5, 1.5, math.Inf(1), math.Inf(-1)} {
		v, rec := ClampConfidence(VerdictAuto, c, 0.60)
		if v != VerdictApprove || !rec.Clamped {
			t.Fatalf("out-of-range confidence %v: got %q clamped=%v, want fail-closed approve", c, v, rec.Clamped)
		}
	}
}

// TestClampConfidence_NoGate — a zero/unset/NaN threshold means no confidence gate (auto passes through).
func TestClampConfidence_NoGate(t *testing.T) {
	for _, thr := range []float64{0, -1, math.NaN()} {
		if v, rec := ClampConfidence(VerdictAuto, 0.01, thr); v != VerdictAuto || rec.Clamped {
			t.Fatalf("threshold %v: got %q clamped=%v, want no gate (auto retained)", thr, v, rec.Clamped)
		}
	}
}

// TestRefine_ConfidenceThenRate — Refine applies both clamps; a low-confidence auto never consumes a rate slot.
func TestRefine_ConfidenceThenRate(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g := NewRateGovernor(func() time.Time { return base })
	params := Params{MinConfidence: f64(0.60), RateLimit: intp(2)}

	// A below-threshold auto clamps on confidence and must NOT charge the rate budget.
	v, rec := g.Refine(VerdictAuto, 0.30, params, "svc.restart")
	if v != VerdictApprove || !rec.ConfidenceClamped || rec.RateClamped {
		t.Fatalf("low-confidence refine = %q rec=%+v, want confidence-clamped approve with no rate charge", v, rec)
	}
	// Two high-confidence autos are admitted (budget was untouched by the clamped one above) ...
	for i := 0; i < 2; i++ {
		if v, _ := g.Refine(VerdictAuto, 0.95, params, "svc.restart"); v != VerdictAuto {
			t.Fatalf("admitted auto %d = %q, want auto", i, v)
		}
	}
	// ... and the 3rd (over the 2/window cap) is rate-clamped.
	if v, rec := g.Refine(VerdictAuto, 0.95, params, "svc.restart"); v != VerdictApprove || !rec.RateClamped {
		t.Fatalf("over-cap refine = %q rec=%+v, want rate-clamped approve", v, rec)
	}
}

// TestRefine_NoKnobs — unset min_confidence and rate_limit leave the verdict untouched.
func TestRefine_NoKnobs(t *testing.T) {
	g := NewRateGovernor(nil)
	if v, rec := g.Refine(VerdictAuto, 0.01, Params{}, "svc.restart"); v != VerdictAuto || rec.ConfidenceClamped || rec.RateClamped {
		t.Fatalf("no-knob refine = %q rec=%+v, want auto retained", v, rec)
	}
}

// TestRefine_OnlyTightens_Property — property: for ANY input, verdictRank(out) >= verdictRank(in). Neither
// clamp may ever loosen a verdict.
func TestRefine_OnlyTightens_Property(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g := NewRateGovernor(func() time.Time { return base })
	rng := rand.New(rand.NewSource(1))
	verdicts := []Verdict{VerdictAuto, VerdictApprove, VerdictDeny, Verdict("bogus")}
	confs := []float64{-1, 0, 0.3, 0.6, 0.95, 1.0, 1.5, math.NaN()}

	for i := 0; i < 5000; i++ {
		in := verdicts[rng.Intn(len(verdicts))]
		conf := confs[rng.Intn(len(confs))]
		var p Params
		if rng.Intn(2) == 0 {
			p.MinConfidence = f64(rng.Float64())
		}
		if rng.Intn(2) == 0 {
			p.RateLimit = intp(rng.Intn(4)) // 0..3, including the no-governor 0
		}
		out, _ := g.Refine(in, conf, p, "cls")
		if verdictRank(out) < verdictRank(in) {
			t.Fatalf("refine LOOSENED: in=%q(%d) out=%q(%d) conf=%v params=%+v",
				in, verdictRank(in), out, verdictRank(out), conf, p)
		}
	}
}

// TestClampConfidence_OnlyTightens_Property — the pure confidence clamp alone never loosens.
func TestClampConfidence_OnlyTightens_Property(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	verdicts := []Verdict{VerdictAuto, VerdictApprove, VerdictDeny, Verdict("x")}
	for i := 0; i < 5000; i++ {
		in := verdicts[rng.Intn(len(verdicts))]
		conf := rng.Float64()*3 - 1 // -1 .. 2
		thr := rng.Float64()*1.2 - 0.1
		out, _ := ClampConfidence(in, conf, thr)
		if verdictRank(out) < verdictRank(in) {
			t.Fatalf("confidence clamp LOOSENED: in=%q out=%q conf=%v thr=%v", in, out, conf, thr)
		}
	}
}
