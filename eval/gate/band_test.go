package gate

import (
	"math"
	"testing"
)

func TestDimSigma2(t *testing.T) {
	// Measured per-dim variances are returned as-is; a dim with too-thin history (or an unknown one) falls
	// back to the documented global.
	cases := []struct {
		dim  string
		want float64
	}{
		{"appropriate_band", 0.617},
		{"correct_diagnosis", 0.648},
		{"evidence_grounded", 0.590},
		{"sensible_proposal", 0.667},
		{"falsifiable_prediction", 0.133},
		{"diagnosis_grounded", globalDimVariance}, // per-dim history too thin -> global
		{"estate_grounded", globalDimVariance},
		{"some_future_dim", globalDimVariance}, // unknown -> global
	}
	for _, c := range cases {
		if got := dimSigma2(c.dim); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("dimSigma2(%q) = %.4f, want %.4f", c.dim, got, c.want)
		}
	}
}

func TestBandHalfWidthCalibratedAndShrinks(t *testing.T) {
	// band = BandZ * sqrt(sigma2/nB + sigma2/nC), sigma2 = the MEASURED per-dim variance. Hand-computed:
	ab20 := bandHalfWidth("appropriate_band", 20, 20)      // 1.645*sqrt(0.617*2/20) = 0.40861
	ab60 := bandHalfWidth("appropriate_band", 60, 60)      // 1.645*sqrt(0.617*2/60) = 0.23591
	fp20 := bandHalfWidth("falsifiable_prediction", 20, 20) // 1.645*sqrt(0.133*2/20) = 0.18971
	for _, tc := range []struct {
		got, want float64
		name      string
	}{
		{ab20, 0.40861, "appropriate_band n=20"},
		{ab60, 0.23591, "appropriate_band n=60"},
		{fp20, 0.18971, "falsifiable_prediction n=20"},
	} {
		if math.Abs(tc.got-tc.want) > 1e-4 {
			t.Errorf("bandHalfWidth %s = %.5f, want %.5f", tc.name, tc.got, tc.want)
		}
	}
	// The low-variance dim (falsifiable 0.133) has a MUCH tighter band than the main dims (~0.6) at the same
	// n — the whole reason per-dim beats a single global: a global 0.63 would UNMEASURE ~every drop on the one
	// dim TG-500 exists for.
	if !(fp20 < ab20) {
		t.Errorf("falsifiable band %.4f must be tighter than appropriate_band %.4f at the same n", fp20, ab20)
	}
	// Shrinks with n (the anti-fail-open lever — pooling sums the per-dim n).
	if !(ab20 > ab60) {
		t.Errorf("band must shrink with n: n=20 %.4f, n=60 %.4f", ab20, ab60)
	}
	// No samples in either arm => unbounded resolution (caller treats as UNMEASURED).
	if !math.IsInf(bandHalfWidth("appropriate_band", 0, 5), 1) || !math.IsInf(bandHalfWidth("appropriate_band", 5, 0), 1) {
		t.Error("bandHalfWidth with a zero-sample arm must be +Inf")
	}
}

func TestCertifiesRegressionDirection(t *testing.T) {
	// band-replaces-floor: a drop is a certified regression only when it EXCEEDS the band (delta < -band). A
	// WIDER band makes a FAIL LESS likely (anti-false-FAIL). The floor is NOT part of this decision — it gates
	// PASS in Compare, not FAIL.
	cases := []struct {
		delta, band float64
		want        bool
		why         string
	}{
		{-0.50, 0.40, true, "0.50 drop exceeds the 0.40 band -> FAIL"},
		{-0.30, 0.40, false, "0.30 drop within the 0.40 band -> not certified"},
		{-0.41, 0.40, true, "just past the band -> FAIL"},
		{-0.39, 0.40, false, "just within the band -> not certified"},
		{-0.50, 0.60, false, "same 0.50 drop but a wider 0.60 band absorbs it -> not certified"},
		{0.10, 0.40, false, "an improvement is never a regression"},
	}
	for _, c := range cases {
		if got := certifiesRegression(c.delta, c.band); got != c.want {
			t.Errorf("certifiesRegression(Δ%.2f, band%.2f) = %v, want %v — %s", c.delta, c.band, got, c.want, c.why)
		}
	}
	// Monotone in the band: widening can only turn a certified FAIL into not-certified, never back.
	prev := true
	for band := 0.0; band <= 2.0; band += 0.05 {
		cert := certifiesRegression(-0.6, band)
		if cert && !prev {
			t.Errorf("certifiesRegression not monotone: band %.2f re-certified after a wider band absorbed it", band)
		}
		prev = cert
	}
}
