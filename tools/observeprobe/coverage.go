package observeprobe

// CoverageResult is the "coverage of the unmeasured" scorecard dimension (TG-180): of the census-UNOBSERVABLE
// entities, how many has the probe actually CONFIRMED a verdict on, versus how many remain untested.
//
// The denominator and numerator are computed from the SAME current unobservable set — the denominator IS that
// set, and the numerator is counted by intersecting it with the probe-confirmed hosts — so they share one
// freshness. A host that LEAVES the unobservable set (e.g. a probe made it alert, so the next census reads it
// observed) leaves BOTH the denominator and the numerator at once, and can never inflate the ratio. That is the
// coverage-denominator discipline: a denominator counting a different, staler population than the numerator
// reads healthy exactly during the drift it should catch.
type CoverageResult struct {
	Unobservable int // denominator: distinct census-unobservable entities RIGHT NOW
	Confirmed    int // numerator: those with a terminal probe verdict (observable OR unobservable-confirmed)
	Unprobed     int // Unobservable - Confirmed: the still-unmeasured remainder (surfaced, never implied)
}

// Ratio is Confirmed/Unobservable, or 0 when there is nothing to measure. An empty denominator is honestly "0
// coverage of nothing", never a divide-by-zero and never a phantom 1.0 that would read as full coverage the
// moment the census happens to find no blind spots.
func (c CoverageResult) Ratio() float64 {
	if c.Unobservable <= 0 {
		return 0
	}
	return float64(c.Confirmed) / float64(c.Unobservable)
}

// Coverage intersects the CURRENT census-unobservable set with the probe-confirmed host set. Blank and
// duplicate host names are collapsed so the denominator is DISTINCT entities (matching observe.Census.Total()),
// and only confirmed hosts that are STILL unobservable count toward the numerator — a host that has left the
// unobservable set is not double-counted as "measured coverage" of a set it is no longer in.
func Coverage(unobservable []string, confirmed map[string]bool) CoverageResult {
	seen := map[string]bool{}
	var res CoverageResult
	for _, h := range unobservable {
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		res.Unobservable++
		if confirmed[h] {
			res.Confirmed++
		}
	}
	res.Unprobed = res.Unobservable - res.Confirmed
	return res
}
