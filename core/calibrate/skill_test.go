package calibrate

import (
	"math"
	"testing"
)

func approxAt(t *testing.T, got, want float64, tol float64, what string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s = %.6f, want %.6f (±%g)", what, got, want, tol)
	}
}

// TestSkillScoreIsMeasuredAgainstTheBaseRateNotACoin pins the reference class.
//
// Brier invites comparison against 0.25, which is what always-guessing-0.5 scores — the COIN null. The
// no-skill null is the BASE RATE: a forecaster who ignores every input and always states the observed
// frequency. Scoring against the wrong null changes the verdict, so the reference is asserted numerically
// rather than described in a comment.
func TestSkillScoreIsMeasuredAgainstTheBaseRateNotACoin(t *testing.T) {
	t.Parallel()
	// 4 samples, exactly 1 clean => base rate 0.25, so BrierBase = 0.25*0.75 = 0.1875.
	// A forecaster stating 0.25 every time scores exactly BrierBase and must therefore have skill 0.
	s := []Sample{
		{Confidence: 0.25, Clean: true},
		{Confidence: 0.25, Clean: false},
		{Confidence: 0.25, Clean: false},
		{Confidence: 0.25, Clean: false},
	}
	r := Compute(s, 10)
	approxAt(t, r.BaseRate, 0.25, 1e-9, "BaseRate")
	approxAt(t, r.Brier, 0.1875, 1e-9, "Brier")
	if !r.SkillDefined {
		t.Fatal("skill must be defined when the base rate is neither 0 nor 1")
	}
	approxAt(t, r.SkillScore, 0.0, 1e-9, "SkillScore of a base-rate forecaster")

	// The same Brier judged against the COIN null (0.25) would read as "better than a coin" — which is the
	// misreading this score exists to prevent. Assert the two references genuinely disagree here.
	if r.Brier >= 0.25 {
		t.Fatalf("fixture no longer separates the two nulls: Brier %.4f is not below the coin reference", r.Brier)
	}
}

// TestSkillScoreGoesNegativeWhenConfidenceIsWorseThanSayingNothing is the live case.
func TestSkillScoreGoesNegativeWhenConfidenceIsWorseThanSayingNothing(t *testing.T) {
	t.Parallel()
	// Confident and wrong: states 0.9 on outcomes that are clean only 25% of the time.
	s := []Sample{
		{Confidence: 0.9, Clean: true},
		{Confidence: 0.9, Clean: false},
		{Confidence: 0.9, Clean: false},
		{Confidence: 0.9, Clean: false},
	}
	r := Compute(s, 10)
	if !r.SkillDefined {
		t.Fatal("skill must be defined at base rate 0.25")
	}
	if r.SkillScore >= 0 {
		t.Fatalf("stating 0.9 against a 25%% base rate must score NEGATIVE skill; got %.4f", r.SkillScore)
	}
	// Brier = (0.01 + 3*0.81)/4 = 0.61; BrierBase = 0.1875; skill = 1 - 0.61/0.1875 = -2.2533
	approxAt(t, r.SkillScore, 1-0.61/0.1875, 1e-9, "SkillScore")
}

// TestSkillIsWithheldWhenTheBaseRateIsDegenerate — with every outcome identical there is nothing for a
// forecast to distinguish, BrierBase is 0, and the ratio is undefined. Publishing 0 there would read as
// "no skill" when the truth is "unmeasurable", the same confusion REQ-2022 withholds scores at N=0 to avoid.
func TestSkillIsWithheldWhenTheBaseRateIsDegenerate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		clean bool
	}{{"all clean", true}, {"none clean", false}} {
		t.Run(tc.name, func(t *testing.T) {
			s := []Sample{
				{Confidence: 0.8, Clean: tc.clean},
				{Confidence: 0.6, Clean: tc.clean},
			}
			r := Compute(s, 10)
			if r.SkillDefined {
				t.Fatalf("skill must be UNDEFINED at a degenerate base rate %.1f", r.BaseRate)
			}
			if r.SkillScore != 0 {
				t.Fatalf("an undefined skill must stay at the zero value, got %.4f", r.SkillScore)
			}
		})
	}
}

// TestBaseRateAndSkillAreZeroValuedOnAnEmptySampleSet — N=0 keeps the existing honest-silence contract.
func TestBaseRateAndSkillAreZeroValuedOnAnEmptySampleSet(t *testing.T) {
	t.Parallel()
	r := Compute(nil, 10)
	if r.N != 0 || r.BaseRate != 0 || r.SkillDefined || r.SkillScore != 0 {
		t.Fatalf("empty sample set must yield the zero value, got N=%d base=%.4f defined=%v skill=%.4f",
			r.N, r.BaseRate, r.SkillDefined, r.SkillScore)
	}
}

// TestReproducesTheLiveMeasurement is a regression pin against the real population, so a change to Compute
// that silently moves the published numbers has to face the values actually served on 2026-07-27.
func TestReproducesTheLiveMeasurement(t *testing.T) {
	t.Parallel()
	obs := []struct {
		conf     float64
		n, clean int
	}{
		{0.55, 2, 0}, {0.6, 3, 0}, {0.62, 3, 1}, {0.64, 1, 0}, {0.65, 3, 1}, {0.7, 5, 1},
		{0.75, 4, 1}, {0.76, 1, 1}, {0.78, 5, 2}, {0.8, 13, 5}, {0.82, 1, 0}, {0.85, 14, 5}, {0.9, 9, 0},
	}
	var s []Sample
	for _, o := range obs {
		for i := 0; i < o.n; i++ {
			s = append(s, Sample{Confidence: o.conf, Clean: i < o.clean})
		}
	}
	r := Compute(s, 10)
	if r.N != 64 {
		t.Fatalf("N = %d, want the live 64", r.N)
	}
	approxAt(t, r.Brier, 0.4633, 1e-4, "live Brier")
	approxAt(t, r.ECE, 0.5114, 1e-4, "live ECE")
	approxAt(t, r.MCE, 0.9000, 1e-4, "live MCE")
	approxAt(t, r.BaseRate, 0.2656, 1e-4, "live BaseRate")
	approxAt(t, r.SkillScore, -1.3752, 1e-4, "live SkillScore")
	if r.SkillScore >= 0 {
		t.Fatal("the live measurement must read as NEGATIVE skill — stated confidence carrying less " +
			"information than a constant that looks at nothing")
	}
}
