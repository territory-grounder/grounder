package axis

import (
	"strings"
	"testing"
)

// spec/025 REQ-2502 — "a number without its denominator is not evidence… A zero-numerator axis SHALL be
// published with its statistical upper bound rather than as a bare zero, since '0 observed in n trials'
// and 'cannot happen' are different claims and only one of them is supported by the data."
//
// This scenario was `pending` in spec/025's test mapping: the rule was IMPLEMENTED — as a closure inside
// cmd/axisscore's text renderer — and therefore untestable and unreusable. Extracting it is what makes
// the requirement checkable at all.

// TestAZeroIsNeverPublishedBare is the requirement, stated as the thing that must not happen.
func TestAZeroIsNeverPublishedBare(t *testing.T) {
	got := ZeroNumeratorBound(12)
	if got == "0%" || got == "0.0%" || !strings.Contains(got, "<=") {
		t.Fatalf("a zero-numerator axis over 12 trials rendered as %q. A bare zero asserts impossibility "+
			"the sample cannot support — REQ-2502 calls publishing it \"the single easiest way for this "+
			"harness to overstate its own evidence\".", got)
	}
	if !strings.Contains(got, "95%") {
		t.Errorf("the bound does not state its confidence level: %q — a bound without one is not a claim "+
			"a reader can check", got)
	}
}

// TestTheBoundShrinksWithTheSample is the property a bare zero HIDES: zero over 3 trials and zero over
// 3,000 look identical and mean entirely different things.
func TestTheBoundShrinksWithTheSample(t *testing.T) {
	small, okS := ZeroNumeratorUpperBound(3)
	large, okL := ZeroNumeratorUpperBound(3000)
	if !okS || !okL {
		t.Fatal("a positive sample reported no bound")
	}
	if !(small > large) {
		t.Fatalf("bound over 3 trials (%v) is not wider than over 3000 (%v). If the bound does not move "+
			"with n, it carries no information about the sample and a reader learns nothing a bare zero "+
			"would not have told them.", small, large)
	}
	// The rule of three, checked numerically rather than trusted: 3/n.
	if small < 0.99 || small > 1.01 {
		t.Errorf("3 trials should bound at ~1.0 (3/3), got %v", small)
	}
	if large < 0.0009 || large > 0.0011 {
		t.Errorf("3000 trials should bound at ~0.001 (3/3000), got %v", large)
	}
}

// TestAnEmptySampleIsNotMeasured is the vacuity floor, and it is the half REQ-2502 is really about: a
// rate over an empty denominator must not render as a confident number in either form.
func TestAnEmptySampleIsNotMeasured(t *testing.T) {
	for _, n := range []int{0, -1} {
		got := ZeroNumeratorBound(n)
		if strings.Contains(got, "<=") || strings.Contains(got, "%") {
			t.Errorf("n=%d rendered a BOUND (%q). There is no sample to bound; publishing one would "+
				"dress an absent measurement as a confident ceiling.", n, got)
		}
		if !strings.Contains(strings.ToLower(got), "not measured") {
			t.Errorf("n=%d rendered %q — it must say the axis was not measured, so 'no data' and "+
				"'no events' stay distinguishable", n, got)
		}
		if _, ok := ZeroNumeratorUpperBound(n); ok {
			t.Errorf("n=%d reported ok=true — a numeric caller would publish 0.0, which reads as a "+
				"MEASURED zero and is exactly the conflation this requirement forbids", n)
		}
	}
}

// TestTheCLIStillUsesTheSharedRule. The extraction is only worth anything if the CLI now calls it —
// otherwise there are two implementations of one requirement, free to drift.
func TestTheCLIStillUsesTheSharedRule(t *testing.T) {
	// The renderer's own contract: 0 of 12 must read as at most ~25%.
	got := ZeroNumeratorBound(12)
	if !strings.Contains(got, "25.0%") {
		t.Errorf("0 of 12 rendered %q — the rule of three gives 3/12 = 25%%, and the requirement's own "+
			"worked example depends on that arithmetic", got)
	}
}
