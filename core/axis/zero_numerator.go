// Package axis holds the reporting rules every benchmark axis obeys, extracted so they can be tested
// and reused rather than living as closures inside one CLI.
package axis

import (
	"fmt"
	"math"
)

// zeroNumeratorConfidence is the one-sided confidence level of the published upper bound.
const zeroNumeratorConfidence = 0.95

// ZeroNumeratorBound renders the honest reading of an axis that observed ZERO events in n trials
// (spec/025 REQ-2502, REQ-2533).
//
// "0 observed in n trials" and "cannot happen" are different claims and only one is supported by data.
// Publishing the bare 0% asserts impossibility the sample cannot support — REQ-2502 calls it "the single
// easiest way for this harness to overstate its own evidence".
//
// The bound is the EXACT one-sided 95% Clopper–Pearson upper limit on a binomial proportion with zero
// successes, 1 − 0.05^(1/n): 0 of 1 is "at most 95%", 0 of 2 "at most 77.6%", 0 of 12 "at most 22.1%", and
// driving it under 1% needs 299 clean trials. It replaced the rule-of-three approximation 3/n (TG-670): the
// approximation is only valid for n ≳ 30 and printed "≤150%" for 0 of 2 on the 2026-11-07 current-build
// report — a bound above 100% is not a bound. The exact limit is never above 1 and shrinks with the sample,
// which is exactly the property a bare zero hides: a zero over 3 trials and a zero over 3,000 look identical
// and are not.
//
// MODEL-DEPENDENT, AND SAID SO. The limit assumes independent, identically distributed trials. Heals on a
// handful of guests sharing one hypervisor, one gateway and one deploy are clustered, selected and inspected
// repeatedly, so the real uncertainty is wider than the number and the label carries the assumption. It is a
// DESCRIPTIVE statement beside the raw count, never a gate.
//
// n <= 0 returns "not measured" rather than a bound, because there is no sample to bound. A rate computed
// over an empty denominator is the vacuity this whole requirement exists to prevent, and returning a
// confident-looking "<=inf%" or "0%" for it would reintroduce it at the reporting layer.
func ZeroNumeratorBound(n int) string {
	b, ok := ZeroNumeratorUpperBound(n)
	if !ok {
		return "no sample — not measured"
	}
	return fmt.Sprintf("<=%.1f%% (exact one-sided 95%% Clopper-Pearson upper bound on 0/%d; model-dependent: assumes independent trials)", b*100, n)
}

// ZeroNumeratorUpperBound is the numeric half, for callers that publish a value rather than a sentence
// (a gauge, a JSON field). Returns the bound as a FRACTION in [0,1], and ok=false when there is no
// sample — so a caller cannot accidentally publish 0.0 for "unmeasured", which is the same conflation
// one layer down.
func ZeroNumeratorUpperBound(n int) (bound float64, ok bool) {
	if n <= 0 {
		return 0, false
	}
	return exactZeroUpper(n), true
}

// exactZeroUpper is the one-sided (1 − α) Clopper–Pearson upper limit for k = 0 of n: the p at which
// P(X = 0 | p) = (1 − p)^n equals α, i.e. 1 − α^(1/n). It is in (0, 1) for every n ≥ 1.
func exactZeroUpper(n int) float64 {
	alpha := 1 - zeroNumeratorConfidence
	return 1 - math.Pow(alpha, 1/float64(n))
}
