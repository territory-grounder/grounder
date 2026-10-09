package observeprobe

import "testing"

// The denominator is the DISTINCT current unobservable set; the numerator is those with a terminal verdict.
func TestCoverage_NumeratorAndDenominator(t *testing.T) {
	unobs := []string{"a", "b", "c", "d"}
	confirmed := map[string]bool{"a": true, "c": true}
	got := Coverage(unobs, confirmed)
	if got.Unobservable != 4 || got.Confirmed != 2 || got.Unprobed != 2 {
		t.Fatalf("got %+v, want {Unobservable:4 Confirmed:2 Unprobed:2}", got)
	}
	if got.Ratio() != 0.5 {
		t.Fatalf("ratio = %v, want 0.5", got.Ratio())
	}
}

// Blanks and duplicates collapse, so the denominator is DISTINCT entities (matching observe.Census.Total()).
func TestCoverage_DistinctDenominator(t *testing.T) {
	got := Coverage([]string{"a", "a", "", "b"}, map[string]bool{"a": true})
	if got.Unobservable != 2 || got.Confirmed != 1 {
		t.Fatalf("got %+v, want Unobservable:2 Confirmed:1 (dedup + blank-skip)", got)
	}
}

// FRESHNESS: the numerator is counted ONLY against the current denominator set. A host that has LEFT the
// unobservable set (e.g. a probe made it alert, so the census no longer lists it) is not credited as coverage
// of a set it is no longer in — the denominator and numerator share one freshness, so the ratio cannot be
// inflated by stale confirmations. Here "gone" was confirmed but is absent from the current census: it must
// count toward NEITHER.
func TestCoverage_ConfirmedButNoLongerUnobservable_DoesNotInflate(t *testing.T) {
	unobs := []string{"still-blind"}                             // current census — "gone" has left it
	confirmed := map[string]bool{"gone": true, "still-blind": false} // "gone" was confirmed earlier
	got := Coverage(unobs, confirmed)
	if got.Unobservable != 1 || got.Confirmed != 0 {
		t.Fatalf("got %+v, want Unobservable:1 Confirmed:0 — a stale confirmation must not inflate coverage", got)
	}
}

// An empty denominator is honest 0, never a divide-by-zero or a phantom full coverage.
func TestCoverage_EmptyDenominatorIsZeroNotOne(t *testing.T) {
	got := Coverage(nil, map[string]bool{"x": true})
	if got.Unobservable != 0 || got.Confirmed != 0 || got.Ratio() != 0 {
		t.Fatalf("got %+v ratio=%v, want all zero for an empty census", got, got.Ratio())
	}
}
