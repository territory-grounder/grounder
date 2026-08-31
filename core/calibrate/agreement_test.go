package calibrate

import (
	"math"
	"strconv"
	"testing"
)

// A CALIBRATION HARNESS MUST BE HONEST ON THIN DATA.
//
// The failure this guards is not a wrong formula — it is a harness that returns 0 or panics when nothing has
// been labelled, and so reports a MEASUREMENT where it should report "not enough evidence". Every function
// here is total, and every rate carries the population it was computed over.

func outs(truth, judge []bool) []Outcome {
	o := make([]Outcome, len(truth))
	for i := range truth {
		o[i] = Outcome{Truth: truth[i], Judge: judge[i]}
	}
	return o
}

// approxAgree is named distinctly from reliability_test.go's approx — same package, one namespace.
func approxAgree(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// n=0 is UNCALIBRATED, not failing, and every rate must say it is undefined rather than return a bare zero.
func TestEmptyPopulationIsUndefinedNotZero(t *testing.T) {
	r := Calibrate(nil, 0.70)
	if r.Confusion.N() != 0 {
		t.Fatalf("want an empty table, got %+v", r.Confusion)
	}
	for name, rate := range map[string]Rate{"TPR": r.TPR, "TNR": r.TNR, "precision": r.Precision, "accuracy": r.Accuracy, "kappa": r.Kappa} {
		if rate.Defined {
			t.Errorf("%s must be UNDEFINED at n=0, not a value (%v) that reads as a measurement", name, rate.Value)
		}
	}
	if r.MeetsBar {
		t.Error("an unlabelled judge must never pass the bar")
	}
	if r.BarReason == "" {
		t.Error("a failing bar must say WHY, or it is undiagnosable")
	}
}

// The textbook counts, verified by hand so the implementation cannot bless itself.
func TestConfusionAndRatesAgainstHandComputedValues(t *testing.T) {
	// truth: T T T T F F F F   judge: T T T F F F F T
	// TP=3 FN=1 TN=3 FP=1
	c := Tabulate(outs(
		[]bool{true, true, true, true, false, false, false, false},
		[]bool{true, true, true, false, false, false, false, true}))
	if c.TP != 3 || c.FN != 1 || c.TN != 3 || c.FP != 1 {
		t.Fatalf("hand-computed table mismatch: %+v", c)
	}
	approxAgree(t, TPR(c).Value, 3.0/4.0, "TPR")
	approxAgree(t, TNR(c).Value, 3.0/4.0, "TNR")
	approxAgree(t, Precision(c).Value, 3.0/4.0, "precision")
	approxAgree(t, Accuracy(c).Value, 6.0/8.0, "accuracy")
	// kappa: po=0.75, pe = (4/8)(4/8) + (4/8)(4/8) = 0.5 → (0.75-0.5)/0.5 = 0.5
	approxAgree(t, Kappa(c).Value, 0.5, "kappa")
}

// KAPPA IS THE POINT. Raw agreement flatters a skewed sample: two raters who both almost always say "no"
// agree constantly while sharing no judgement. If kappa did not correct for that, the harness would certify a
// judge that has learned only the base rate.
func TestKappaExposesAgreementThatIsOnlyChance(t *testing.T) {
	// 18 true negatives, and the two raters disagree on both positives — high accuracy, no real agreement.
	truth := []bool{true, false}
	judge := []bool{false, true}
	for i := 0; i < 18; i++ {
		truth = append(truth, false)
		judge = append(judge, false)
	}
	c := Tabulate(outs(truth, judge))
	acc := Accuracy(c)
	k := Kappa(c)
	if acc.Value < 0.85 {
		t.Fatalf("fixture should have HIGH accuracy, got %v", acc.Value)
	}
	if k.Value >= 0.2 {
		t.Fatalf("kappa must expose that %v accuracy is near-chance agreement; got kappa=%v", acc.Value, k.Value)
	}
}

// A constant rater leaves no room above chance — kappa is undefined, which is a fact about the SAMPLE and
// must not be reported as a value.
func TestKappaUndefinedWhenChanceAgreementIsTotal(t *testing.T) {
	n := 10
	truth := make([]bool, n)
	judge := make([]bool, n) // both always false
	if k := Kappa(Tabulate(outs(truth, judge))); k.Defined {
		t.Errorf("kappa must be undefined when both raters are constant; got %v", k.Value)
	}
}

// THE BAR IS ON THE LOWER BOUND, NOT THE POINT ESTIMATE. A perfect score on a handful of items has not
// demonstrated anything, and this is what stops a thin sample certifying the judge.
func TestBarRequiresTheLowerBoundNotThePointEstimate(t *testing.T) {
	// 4 of 4 and 4 of 4 — a perfect point estimate on a tiny sample.
	small := Calibrate(outs(
		[]bool{true, true, true, true, false, false, false, false},
		[]bool{true, true, true, true, false, false, false, false}), 0.70)
	approxAgree(t, small.TPR.Value, 1.0, "TPR point estimate")
	if small.MeetsBar {
		t.Fatalf("a perfect score on 4+4 items must NOT clear a 0.70 bar — the interval is too wide (TPR lo=%v)", small.TPR.Lo)
	}
	if small.BarReason == "" {
		t.Error("the refusal must be diagnosable")
	}

	// The same perfect performance, at a sample size that actually demonstrates it.
	var truth, judge []bool
	for i := 0; i < 60; i++ {
		truth = append(truth, true)
		judge = append(judge, true)
	}
	for i := 0; i < 60; i++ {
		truth = append(truth, false)
		judge = append(judge, false)
	}
	big := Calibrate(outs(truth, judge), 0.70)
	if !big.MeetsBar {
		t.Fatalf("perfect performance on 60+60 must clear the bar; TPR lo=%v TNR lo=%v reason=%q",
			big.TPR.Lo, big.TNR.Lo, big.BarReason)
	}
}

// A sample with no positives cannot measure sensitivity. That is a property of the sample, and reporting it
// as a failing judge would be a false accusation.
func TestOneSidedSampleCannotMeasureBothRatesAndSaysSo(t *testing.T) {
	r := Calibrate(outs([]bool{false, false, false}, []bool{false, false, false}), 0.70)
	if r.TPR.Defined {
		t.Error("with no ground-truth positives, TPR must be undefined")
	}
	if r.MeetsBar {
		t.Error("an unmeasurable rate must never pass the bar")
	}
	if r.BarReason == "" || !contains(r.BarReason, "positives") {
		t.Errorf("the reason must name the missing class, got %q", r.BarReason)
	}
}

// Wilson, not the normal approximation: at p=0 the textbook interval has ZERO width — "0 observed, therefore
// the rate is exactly 0" — the same overstatement the rule-of-three note guards against elsewhere.
func TestWilsonGivesANonZeroWidthIntervalAtTheMargins(t *testing.T) {
	lo, hi := wilson(0, 10, 1.959963984540054)
	if lo != 0 {
		t.Errorf("lower bound at p=0 must be 0, got %v", lo)
	}
	if hi <= 0 {
		t.Fatalf("upper bound at p=0 must be POSITIVE — 0 of 10 does not prove the rate is zero; got %v", hi)
	}
	lo2, hi2 := wilson(10, 10, 1.959963984540054)
	// At p=1 the half-width is z^2/(2n*denom) and centre+half collapses to denom/denom = 1 exactly — but only
	// in exact arithmetic. Compare with a tolerance rather than for equality: the property is mathematical,
	// the representation is not.
	if math.Abs(hi2-1) > 1e-12 {
		t.Errorf("upper bound at p=1 must be 1, got %v", hi2)
	}
	if lo2 >= 1 {
		t.Fatalf("lower bound at p=1 must be BELOW 1 — 10 of 10 does not prove certainty; got %v", lo2)
	}
	// Wider on less data, always.
	loSmall, hiSmall := wilson(5, 10, 1.959963984540054)
	loBig, hiBig := wilson(50, 100, 1.959963984540054)
	if (hiSmall - loSmall) <= (hiBig - loBig) {
		t.Errorf("the interval must be WIDER at n=10 (%v) than at n=100 (%v)", hiSmall-loSmall, hiBig-loBig)
	}
}

// Stratification: a judge can be fine overall and useless on the one class that matters. Pooling hides it.
func TestStratifyExposesAPerClassCollapseThatPoolingHides(t *testing.T) {
	// class A: perfect (20+20). class B: judge inverts every positive (0 of 10 caught).
	var o []Outcome
	var cls []string
	for i := 0; i < 20; i++ {
		o = append(o, Outcome{Truth: true, Judge: true})
		cls = append(cls, "A")
	}
	for i := 0; i < 20; i++ {
		o = append(o, Outcome{Truth: false, Judge: false})
		cls = append(cls, "A")
	}
	for i := 0; i < 10; i++ {
		o = append(o, Outcome{Truth: true, Judge: false})
		cls = append(cls, "B")
	}
	pooled := Calibrate(o, 0.70)
	reports, keys := StratifyBy(o, func(i int) string { return cls[i] }, 0.70)
	if len(keys) != 2 || keys[0] != "A" || keys[1] != "B" {
		t.Fatalf("strata must be returned sorted for stable reporting, got %v", keys)
	}
	if !reports["A"].MeetsBar {
		t.Errorf("class A is perfect at n=40 and should clear the bar; reason=%q", reports["A"].BarReason)
	}
	if reports["B"].MeetsBar {
		t.Error("class B catches ZERO positives and must fail")
	}
	if pooled.TPR.Value <= reports["B"].TPR.Value {
		t.Errorf("the pooled TPR (%v) should look better than class B's (%v) — that is the hiding this test is about",
			pooled.TPR.Value, reports["B"].TPR.Value)
	}
}

// MUTATION CONTROL. The bar is only load-bearing if it actually reads the lower bound. Perturb the threshold
// across the measured interval and the verdict must flip — if it never flips, the bar is decorative.
func TestMutationControl_TheBarReadsTheLowerBound(t *testing.T) {
	var truth, judge []bool
	for i := 0; i < 60; i++ {
		truth = append(truth, true)
		judge = append(judge, true)
	}
	for i := 0; i < 60; i++ {
		truth = append(truth, false)
		judge = append(judge, false)
	}
	o := outs(truth, judge)
	lo := Calibrate(o, 0.70).TPR.Lo
	if lo <= 0.70 || lo >= 1.0 {
		t.Fatalf("fixture must sit strictly between the two thresholds under test; TPR lo=%v", lo)
	}
	if !Calibrate(o, 0.70).MeetsBar {
		t.Error("must PASS a threshold below the lower bound")
	}
	if Calibrate(o, 0.999).MeetsBar {
		t.Fatal("must FAIL a threshold above the lower bound — if it passes, the bar is not reading the interval")
	}
	t.Logf("mutation control holds: lower bound %.4f passes 0.70 and fails 0.999", lo)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

var _ = strconv.Itoa

// A POINT ESTIMATE MUST NOT WEAR THE COSTUME OF AN INTERVAL.
//
// Kappa returned Lo = Hi = the estimate, which every renderer then printed as "95% CI k–k" — the tightest
// possible interval, on the least certain number in the report. A reader cannot distinguish that from a
// genuinely precise estimate.
func TestKappaReportsNoIntervalRatherThanADegenerateOne(t *testing.T) {
	k := Kappa(Confusion{TP: 50, FP: 10, TN: 30, FN: 10})
	if !k.Defined {
		t.Fatal("kappa is defined for this table")
	}
	if !k.NoInterval {
		t.Fatal("kappa must declare that it carries NO interval, so renderers omit one instead of printing " +
			"Lo–Hi and implying a precision it never computed")
	}
	if k.Lo != 0 || k.Hi != 0 {
		t.Errorf("an unset interval must stay zero-valued rather than echo the estimate; got Lo=%v Hi=%v", k.Lo, k.Hi)
	}
}

// The proportions DO carry a real Wilson interval — the fix must not strip intervals from statistics that
// legitimately have them.
func TestProportionsStillCarryTheirWilsonInterval(t *testing.T) {
	for name, r := range map[string]Rate{
		"TPR":       TPR(Confusion{TP: 50, FN: 10}),
		"TNR":       TNR(Confusion{TN: 30, FP: 10}),
		"precision": Precision(Confusion{TP: 50, FP: 10}),
		"accuracy":  Accuracy(Confusion{TP: 50, FP: 10, TN: 30, FN: 10}),
	} {
		if r.NoInterval {
			t.Errorf("%s has a genuine Wilson interval and must keep it", name)
		}
		if !(r.Lo < r.Value && r.Value < r.Hi) {
			t.Errorf("%s: want Lo < value < Hi, got %.4f < %.4f < %.4f", name, r.Lo, r.Value, r.Hi)
		}
	}
}
