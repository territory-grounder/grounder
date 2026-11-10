package axis

import (
	"math"
	"testing"
)

// Every value below is computed by hand from the formulas, never read off the implementation.
func TestRateIntervalHandValues(t *testing.T) {
	near := func(a, b float64) bool { return math.Abs(a-b) < 0.0015 }
	cases := []struct {
		k, n         int
		lower, upper float64
		method       string
	}{
		{0, 2, 0, 0.7764, MethodExactOneSided},
		{0, 12, 0, 0.2209, MethodExactOneSided},
		{0, 299, 0, 0.00997, MethodExactOneSided},
		{2, 2, 0.2236, 1, MethodExactOneSided},
		{27, 30, 0.7438, 0.9654, MethodWilson},
		{15, 30, 0.3315, 0.6685, MethodWilson},
		{1, 30, 0.0059, 0.1667, MethodWilson},
	}
	for _, c := range cases {
		iv := RateInterval(c.k, c.n)
		if iv == nil {
			t.Fatalf("%d/%d: nil", c.k, c.n)
		}
		if iv.Method != c.method || !near(iv.Lower, c.lower) || !near(iv.Upper, c.upper) {
			t.Errorf("%d/%d: got [%.4f, %.4f] %s, want [%.4f, %.4f] %s", c.k, c.n, iv.Lower, iv.Upper, iv.Method, c.lower, c.upper, c.method)
		}
		if iv.Lower < 0 || iv.Upper > 1 || iv.Lower > iv.Upper {
			t.Errorf("%d/%d: bounds outside [0,1] or inverted: %+v", c.k, c.n, iv)
		}
		if iv.Note == "" || iv.Text() == "" {
			t.Errorf("%d/%d: an interval without its model label is a number nobody can read", c.k, c.n)
		}
	}
}

// No sample → no interval (never a 0% or a 100% standing in for "unknown"); impossible k → nil too.
func TestRateIntervalRefusesTheUnmeasured(t *testing.T) {
	for _, c := range [][2]int{{0, 0}, {1, 0}, {-1, 5}, {6, 5}} {
		if iv := RateInterval(c[0], c[1]); iv != nil {
			t.Errorf("%d/%d: got %+v, want nil", c[0], c[1], iv)
		}
	}
	if (*Interval)(nil).Text() != "" {
		t.Fatal("a nil interval must render as nothing")
	}
}

// MUTATION CONTROL: the zero-numerator bound must agree with the A7 note's ZeroNumeratorBound (one formula, two
// renderings), and it must shrink monotonically with n — the rule-of-three it replaced did not stay ≤ 100%.
func TestZeroNumeratorIntervalAgreesWithTheA7NoteAndShrinks(t *testing.T) {
	prev := 1.0
	for n := 1; n <= 400; n *= 2 {
		iv := RateInterval(0, n)
		if iv.Upper >= prev || iv.Upper > 1 {
			t.Fatalf("n=%d: upper %.4f not below %.4f (or above 1)", n, iv.Upper, prev)
		}
		prev = iv.Upper
		if math.Abs(iv.Upper-exactZeroUpper(n)) > 1e-12 {
			t.Fatalf("n=%d: the interval and the A7 note disagree on the zero-numerator bound", n)
		}
	}
}
