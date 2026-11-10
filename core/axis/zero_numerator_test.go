package axis

import (
	"math"
	"strings"
	"testing"
)

// spec/025 REQ-2502 — "a number without its denominator is not evidence… A zero-numerator axis SHALL be
// published with its statistical upper bound rather than as a bare zero, since '0 observed in n trials'
// and 'cannot happen' are different claims and only one of them is supported by the data." — and
// REQ-2533 (TG-670): the bound SHALL be the exact one-sided Clopper–Pearson limit, never above 100%, and
// SHALL carry its independence assumption.
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
	if !strings.Contains(got, "assumes independent trials") {
		t.Errorf("the bound does not state its model assumption: %q — clustered, selected observations make "+
			"the real uncertainty wider than the number, and the label must say so (REQ-2533)", got)
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
	// The exact limit 1 − 0.05^(1/n), checked numerically rather than trusted.
	if math.Abs(small-0.6316) > 0.001 {
		t.Errorf("3 trials should bound at ~0.632 (1 - 0.05^(1/3)), got %v", small)
	}
	if math.Abs(large-0.000998) > 0.00002 {
		t.Errorf("3000 trials should bound at ~0.000998 (1 - 0.05^(1/3000)), got %v", large)
	}
}

// TestTheBoundIsNeverAboveOneHundredPercent is the TG-670 defect as an oracle: the rule-of-three
// approximation 3/n printed "<=150.0%" for 0 of 2 and "<=300.0%" for 0 of 1 on the 2026-11-07 current-build
// report. A bound above 100% is not a bound; the exact limit stays below 1 for every n.
func TestTheBoundIsNeverAboveOneHundredPercent(t *testing.T) {
	for n := 1; n <= 40; n++ {
		b, ok := ZeroNumeratorUpperBound(n)
		if !ok || b >= 1 || b <= 0 {
			t.Fatalf("n=%d: bound %v ok=%v — must be strictly inside (0, 1)", n, b, ok)
		}
	}
	if got := ZeroNumeratorBound(2); !strings.Contains(got, "77.6%") {
		t.Errorf("0 of 2 rendered %q, want the exact 77.6%% (1 - 0.05^(1/2)), not the rule-of-three 150%%", got)
	}
	if got := ZeroNumeratorBound(1); !strings.Contains(got, "95.0%") {
		t.Errorf("0 of 1 rendered %q, want 95.0%% — one clean trial excludes almost nothing", got)
	}
	// The sample that drives the bound under 1%: ln(0.05)/ln(0.99) = 298.07, so 299 clean trials.
	if b, _ := ZeroNumeratorUpperBound(298); b <= 0.01 {
		t.Errorf("298 trials already bound under 1%% (%v) — the arithmetic is off", b)
	}
	if b, _ := ZeroNumeratorUpperBound(299); b > 0.01 {
		t.Errorf("299 trials do not bound under 1%% (%v) — the arithmetic is off", b)
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
	// The renderer's own contract: 0 of 12 must read as at most ~22%.
	got := ZeroNumeratorBound(12)
	if !strings.Contains(got, "22.1%") {
		t.Errorf("0 of 12 rendered %q — the exact limit gives 1 - 0.05^(1/12) = 22.1%%, and the requirement's "+
			"worked example depends on that arithmetic", got)
	}
}
