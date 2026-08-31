package gate

// TG-500 invariant 3 — the per-dimension UNMEASURED-rate trend, the anti-erosion guard. A dimension that is
// CHRONICALLY relabelled UNMEASURED (its drop is always within the band, never certifiable) is the signal the
// band is too coarse for that dimension and the evidence to upgrade its σ² to a per-dim empirical/shrinkage
// estimate has arrived (the staged calibration plan). Making the rate VISIBLE is what turns a slow, invisible
// erosion — every run quietly relabelling a real regression "unmeasured" and the gate certifying around it —
// into a tracked number an operator can act on.
//
// It is OBSERVABILITY-ONLY: nothing in Compare/resolveOutcome reads it, so it can never move a verdict. It is
// computed from the archived Verdicts' per-dim Unresolved flags (already persisted in every eval/history
// verdict.json, and now backed by IndividualRun.DimMeans for σ² re-derivation), so the trend history IS the
// register — no separate mutable state to keep honest.

// UnmeasuredDims returns the dimensions this verdict left UNMEASURED (a drop past the floor but within the
// band, or below min-N). The per-run input to the trend. PER-DIM, never the overall.
func (v *Verdict) UnmeasuredDims() []string {
	var out []string
	for _, d := range v.Dims {
		if d.Unresolved {
			out = append(out, d.Dim)
		}
	}
	return out
}

// UnmeasuredRateByDim computes, over a window of past verdicts, the fraction of runs in which each dimension
// went UNMEASURED. The denominator is the runs in which the dimension was PRESENT (had a DimResult), so a
// dimension absent from some cards is not diluted. A rate trending toward 1.0 for a dimension is the trigger
// to refine that dimension's σ² from the captured per-dim-per-run history — not to loosen the gate.
func UnmeasuredRateByDim(verdicts []Verdict) map[string]float64 {
	present := map[string]int{}
	unmeasured := map[string]int{}
	for _, v := range verdicts {
		for _, d := range v.Dims {
			present[d.Dim]++
			if d.Unresolved {
				unmeasured[d.Dim]++
			}
		}
	}
	rate := make(map[string]float64, len(present))
	for dim, n := range present {
		if n > 0 {
			rate[dim] = round2(float64(unmeasured[dim]) / float64(n))
		}
	}
	return rate
}
