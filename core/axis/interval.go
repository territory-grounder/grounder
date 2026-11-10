package axis

import (
	"fmt"
	"math"
)

// DESCRIPTIVE INTERVALS (TG-672, G4A; owner prompt 2026-11-07 "Repair the scorecard" 5). Raw counts come first and
// are what the bar gates on; an interval is printed BESIDE them, labelled as conditional on its model, and never
// decides anything. The model is "n independent Bernoulli trials", which a soak violates in known ways (the same
// guests faulted repeatedly, the same checks polled, non-random fault order), so the label says so every time.
// No bound is ever above 100% or below 0%: the rule-of-three "≤150%" that the old A7 note produced for 0/2 is
// exactly the kind of number this replaces.

// Interval is a two-sided or one-sided 95% interval for a rate k/n.
type Interval struct {
	Lower  float64 `json:"lower"`
	Upper  float64 `json:"upper"`
	Method string  `json:"method"`
	Note   string  `json:"note"`
}

// Interval methods.
const (
	MethodWilson        = "wilson-95-two-sided"
	MethodExactOneSided = "clopper-pearson-95-one-sided"
	intervalNote        = "model-dependent: assumes n independent trials; clustered, non-random, repeatedly inspected observations violate it; descriptive only, never a gate"
	z95                 = 1.959963984540054
)

// RateInterval returns the descriptive 95% interval for k successes in n trials, or nil when n <= 0 (no sample has
// no interval). 0 < k < n → two-sided Wilson score interval; k == 0 → exact one-sided upper bound 1 − 0.05^(1/n)
// with lower 0; k == n → exact one-sided lower bound 0.05^(1/n) with upper 1. Bounds are clamped to [0, 1].
func RateInterval(k, n int) *Interval {
	if n <= 0 || k < 0 || k > n {
		return nil
	}
	iv := &Interval{Note: intervalNote}
	switch {
	case k == 0:
		iv.Method, iv.Lower, iv.Upper = MethodExactOneSided, 0, exactZeroUpper(n)
	case k == n:
		iv.Method, iv.Lower, iv.Upper = MethodExactOneSided, math.Pow(0.05, 1/float64(n)), 1
	default:
		p, nf := float64(k)/float64(n), float64(n)
		denom := 1 + z95*z95/nf
		centre := (p + z95*z95/(2*nf)) / denom
		half := z95 * math.Sqrt(p*(1-p)/nf+z95*z95/(4*nf*nf)) / denom
		iv.Method, iv.Lower, iv.Upper = MethodWilson, centre-half, centre+half
	}
	iv.Lower = math.Max(0, math.Min(1, iv.Lower))
	iv.Upper = math.Max(0, math.Min(1, iv.Upper))
	return iv
}

// Text renders the interval for a cell note: "95% [a%, b%] (<method>)".
func (iv *Interval) Text() string {
	if iv == nil {
		return ""
	}
	return fmt.Sprintf("95%% [%.1f%%, %.1f%%] (%s)", iv.Lower*100, iv.Upper*100, iv.Method)
}
