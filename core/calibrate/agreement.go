package calibrate

import (
	"math"
	"sort"
)

// JUDGE CALIBRATION — does the judge agree with ground truth?
//
// TG's head-to-head verdicts are produced by an LLM judge whose agreement with reality has never been
// measured: before this shipped, `TPR`, `TNR`, `kappa`, `Wilson` and `Wilcoxon` appeared NOWHERE in the Go
// tree. A systematically wrong judge applied fairly to both systems still yields a wrong comparison, so an
// uncalibrated judge cannot support a claim that one system diagnoses better than another — which is exactly
// the claim the diagnosis-gap phase exists to make or to honestly withdraw.
//
// The functions here are pure and total. Every one is defined at n=0 and at the degenerate margins (all
// positive, all negative, perfect agreement, zero agreement), because a calibration harness that panics or
// silently returns 0 on thin data is worse than none: it reports a number where it should report "not enough
// evidence". Each result therefore carries its own n and an explicit Defined flag rather than leaning on a
// zero value that reads like a measurement.
//
// Provenance: [O] INV-22 (no undeclared test-gap; a number without its population is not evidence) ·
// spec/025 REQ-2502 (a zero-numerator axis is published with its bound, never as a bare zero).

// Outcome is one judged item scored against ground truth. Both fields are booleans because the calibration
// question is deliberately BINARY — "did the judge call this the same way the ground truth does" — which is
// what makes a label a 15-30 second confirmation rather than an essay.
type Outcome struct {
	Truth bool // the ground-truth label
	Judge bool // what the judge said
}

// Confusion is the 2x2 contingency table over judged items.
type Confusion struct {
	TP, FP, TN, FN int
}

// N is the total number of judged items.
func (c Confusion) N() int { return c.TP + c.FP + c.TN + c.FN }

// Tabulate builds the contingency table. An empty input yields a zero table whose every derived rate reports
// Defined=false — the honest answer to "how good is the judge" when nothing has been labelled.
func Tabulate(out []Outcome) Confusion {
	var c Confusion
	for _, o := range out {
		switch {
		case o.Truth && o.Judge:
			c.TP++
		case !o.Truth && o.Judge:
			c.FP++
		case !o.Truth && !o.Judge:
			c.TN++
		default:
			c.FN++
		}
	}
	return c
}

// Rate is a proportion reported WITH the population it was computed over and a Wilson score interval. A rate
// is never returned bare: 1.0 from one observation and 1.0 from four hundred are different claims, and only
// the interval distinguishes them.
type Rate struct {
	// NoInterval marks a statistic reported as a POINT ESTIMATE with no confidence interval. Renderers MUST
	// omit the interval rather than print Lo–Hi, which for such a statistic would be a fabricated range.
	NoInterval bool
	Value      float64 // the point estimate; meaningless unless Defined
	Lo, Hi     float64 // Wilson score interval at the requested confidence
	N          int     // the denominator this was computed over
	Defined    bool    // false when N == 0 — the rate does not exist, it is not zero
}

// TPR is sensitivity: of the items ground truth calls positive, how many did the judge catch. Undefined when
// there are no positives to catch — which is a property of the SAMPLE, not of the judge, and must not be
// reported as a failure.
func TPR(c Confusion) Rate { return rate(c.TP, c.TP+c.FN) }

// TNR is specificity: of the items ground truth calls negative, how many did the judge correctly reject.
func TNR(c Confusion) Rate { return rate(c.TN, c.TN+c.FP) }

// Precision is of the items the judge called positive, how many truly were.
func Precision(c Confusion) Rate { return rate(c.TP, c.TP+c.FP) }

// Accuracy is the overall agreement fraction. It is reported for completeness and is the WEAKEST of these
// numbers: on a skewed sample a judge that always answers the majority class scores well while being useless,
// which is precisely why the exit criterion names TPR and TNR separately and adds kappa.
func Accuracy(c Confusion) Rate { return rate(c.TP+c.TN, c.N()) }

func rate(num, den int) Rate {
	if den <= 0 {
		return Rate{Defined: false}
	}
	p := float64(num) / float64(den)
	lo, hi := wilson(num, den, 1.959963984540054) // z for 95%
	return Rate{Value: p, Lo: lo, Hi: hi, N: den, Defined: true}
}

// wilson returns the Wilson score interval, used instead of the normal approximation because the interesting
// cases here are exactly the ones it handles and the normal approximation does not: small n, and proportions
// at 0 or 1. The textbook interval gives a zero-width interval at p=0 — "0 failures observed, therefore the
// rate is exactly 0" — which is the same overstatement the rule-of-three note guards against elsewhere.
func wilson(num, den int, z float64) (float64, float64) {
	n := float64(den)
	p := float64(num) / n
	z2 := z * z
	denom := 1 + z2/n
	centre := (p + z2/(2*n)) / denom
	half := (z / denom) * math.Sqrt(p*(1-p)/n+z2/(4*n*n))
	lo, hi := centre-half, centre+half
	return math.Max(0, lo), math.Min(1, hi)
}

// Kappa is Cohen's kappa: agreement corrected for the agreement expected by chance. It is reported because
// raw agreement flatters a skewed sample — two raters who both answer "yes" 95% of the time agree 90% of the
// time while sharing no judgement at all. Kappa is undefined when chance agreement is 1 (both raters gave a
// single constant answer): there is no room above chance to measure, which is a fact about the sample.
func Kappa(c Confusion) Rate {
	n := float64(c.N())
	if n == 0 {
		return Rate{Defined: false}
	}
	po := float64(c.TP+c.TN) / n
	pYes := (float64(c.TP+c.FN) / n) * (float64(c.TP+c.FP) / n)
	pNo := (float64(c.TN+c.FP) / n) * (float64(c.TN+c.FN) / n)
	pe := pYes + pNo
	if 1-pe == 0 {
		return Rate{N: c.N(), Defined: false}
	}
	k := (po - pe) / (1 - pe)
	// NO INTERVAL. Lo and Hi were previously set to k itself, which every renderer then printed as a "95% CI
	// k–k" — a point estimate wearing the costume of an interval, and the tightest-looking one possible. A
	// reader has no way to tell that from a genuinely precise estimate, so it overstates confidence exactly
	// where the number is least certain.
	//
	// Kappa's large-sample standard error is not the Wilson form used for the proportions above (it is a
	// different estimator over the agreement table), so this reports kappa as what it is — a point estimate —
	// rather than fabricate an interval or borrow the wrong one.
	return Rate{Value: k, N: c.N(), Defined: true, NoInterval: true}
}

// Report is the full calibration result for one judged population.
type Report struct {
	Confusion Confusion
	TPR       Rate
	TNR       Rate
	Precision Rate
	Accuracy  Rate
	Kappa     Rate
	// MeetsBar reports whether TPR and TNR both CLEAR the threshold at the LOWER bound of their interval, not
	// at the point estimate. A judge whose TPR is 0.75 on eight items has not demonstrated 0.70; requiring the
	// lower bound is what makes the bar a claim about the judge rather than about the sample size.
	MeetsBar bool
	// BarReason states why MeetsBar is false, so a failing calibration is diagnosable rather than a bare false.
	BarReason string
}

// Calibrate scores a judged population against a threshold that TPR and TNR must both clear.
func Calibrate(out []Outcome, threshold float64) Report {
	c := Tabulate(out)
	r := Report{
		Confusion: c,
		TPR:       TPR(c),
		TNR:       TNR(c),
		Precision: Precision(c),
		Accuracy:  Accuracy(c),
		Kappa:     Kappa(c),
	}
	switch {
	case c.N() == 0:
		r.BarReason = "no labelled items — the judge is UNCALIBRATED, which is not the same as failing"
	case !r.TPR.Defined:
		r.BarReason = "the sample contains no ground-truth positives, so sensitivity cannot be measured"
	case !r.TNR.Defined:
		r.BarReason = "the sample contains no ground-truth negatives, so specificity cannot be measured"
	case r.TPR.Lo < threshold:
		r.BarReason = "TPR lower bound is below the threshold — not demonstrated at this sample size"
	case r.TNR.Lo < threshold:
		r.BarReason = "TNR lower bound is below the threshold — not demonstrated at this sample size"
	default:
		r.MeetsBar = true
	}
	return r
}

// StratifyBy groups outcomes by a caller-supplied key and calibrates each stratum separately, returning the
// keys in sorted order for stable reporting. A judge can be well calibrated overall and badly calibrated on
// the one class that matters; a single pooled number hides that, and pooling across strata is how a
// comparison silently averages two different instruments.
func StratifyBy(out []Outcome, key func(int) string, threshold float64) (map[string]Report, []string) {
	groups := map[string][]Outcome{}
	for i, o := range out {
		k := key(i)
		groups[k] = append(groups[k], o)
	}
	reports := make(map[string]Report, len(groups))
	keys := make([]string, 0, len(groups))
	for k, g := range groups {
		reports[k] = Calibrate(g, threshold)
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return reports, keys
}
