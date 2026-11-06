// Package axis holds the reporting rules every benchmark axis obeys, extracted so they can be tested
// and reused rather than living as closures inside one CLI.
package axis

import "fmt"

// ZeroNumeratorBound renders the honest reading of an axis that observed ZERO events in n trials
// (spec/025 REQ-2502).
//
// "0 observed in n trials" and "cannot happen" are different claims and only one is supported by data.
// Publishing the bare 0% asserts impossibility the sample cannot support — REQ-2502 calls it "the single
// easiest way for this harness to overstate its own evidence".
//
// The 95% upper bound on an unobserved event is the RULE OF THREE: ~3/n. So 0 of 12 is "at most ~25%",
// and driving that under 1% needs ~300 clean trials. The bound shrinks with the sample, which is exactly
// the property a bare zero hides: a zero over 3 trials and a zero over 3,000 look identical and are not.
//
// n <= 0 returns "not measured" rather than a bound, because there is no sample to bound. A rate computed
// over an empty denominator is the vacuity this whole requirement exists to prevent, and returning a
// confident-looking "<=inf%" or "0%" for it would reintroduce it at the reporting layer.
func ZeroNumeratorBound(n int) string {
	if n <= 0 {
		return "no sample — not measured"
	}
	return fmt.Sprintf("<=%.1f%% at 95%% confidence (rule of three: 3/%d)", 300.0/float64(n), n)
}

// ZeroNumeratorUpperBound is the numeric half, for callers that publish a value rather than a sentence
// (a gauge, a JSON field). Returns the bound as a FRACTION in [0,1], and ok=false when there is no
// sample — so a caller cannot accidentally publish 0.0 for "unmeasured", which is the same conflation
// one layer down.
func ZeroNumeratorUpperBound(n int) (bound float64, ok bool) {
	if n <= 0 {
		return 0, false
	}
	return 3.0 / float64(n), true
}
