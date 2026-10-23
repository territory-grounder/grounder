package observe

import (
	"testing"
	"time"
)

// The whole point of TG-180: a silent entity that has fired before is HEALTHY, one that never fired is
// UNOBSERVABLE — the split today's "no alerts == fine" collapses. KILLING MUTATION: treat a missing lastFired
// entry the same as a stale one (drop the `default: Unobservable` arm) and blind-host reads healthy_quiet,
// reddening this.
func TestCensus_ClassifiesThreeStates(t *testing.T) {
	since := time.Unix(1000, 0)
	lastFired := map[string]time.Time{
		"observed-host": time.Unix(1500, 0), // after since → observed
		"quiet-host":    time.Unix(500, 0),  // before since → healthy_quiet (fired, but only in the past)
		// "blind-host" is absent → never fired → unobservable
	}
	res := Census([]string{"observed-host", "quiet-host", "blind-host"}, lastFired, since)
	if got := res.Entities["observed-host"]; got != Observed {
		t.Errorf("observed-host = %s, want observed", got)
	}
	if got := res.Entities["quiet-host"]; got != HealthyQuiet {
		t.Errorf("quiet-host = %s, want healthy_quiet (fired before the window)", got)
	}
	if got := res.Entities["blind-host"]; got != Unobservable {
		t.Errorf("blind-host = %s, want unobservable (never fired) — the split TG-180 exists for", got)
	}
	if res.Counts[Observed] != 1 || res.Counts[HealthyQuiet] != 1 || res.Counts[Unobservable] != 1 {
		t.Errorf("counts = %v, want 1 in each state", res.Counts)
	}
	if res.Total() != 3 {
		t.Errorf("Total() = %d, want 3", res.Total())
	}
}

// An entity that fired EXACTLY at the window start is observed, not quiet (the boundary is inclusive).
func TestCensus_BoundaryAtSinceIsObserved(t *testing.T) {
	since := time.Unix(1000, 0)
	res := Census([]string{"edge"}, map[string]time.Time{"edge": since}, since)
	if got := res.Entities["edge"]; got != Observed {
		t.Errorf("entity fired exactly at `since` = %s, want observed (inclusive boundary)", got)
	}
}

func TestCensus_DedupesAndSkipsBlank(t *testing.T) {
	since := time.Unix(1000, 0)
	res := Census([]string{"h", "h", "", "h"}, map[string]time.Time{"h": time.Unix(1500, 0)}, since)
	if res.Total() != 1 {
		t.Errorf("Total() = %d, want 1 (deduped, blank skipped)", res.Total())
	}
	if res.Counts[Observed] != 1 {
		t.Errorf("observed count = %d, want 1 (a repeated entity is counted once)", res.Counts[Observed])
	}
}

// Every state is always present in Counts (0 when empty), so the coverage metric never drops a bucket.
func TestCensus_CountsCarryEveryStateEvenEmpty(t *testing.T) {
	res := Census(nil, nil, time.Unix(1000, 0))
	for _, st := range CensusStates {
		if _, ok := res.Counts[st]; !ok {
			t.Errorf("Counts missing state %s — an empty bucket must publish 0, not vanish", st)
		}
	}
	if res.Total() != 0 {
		t.Errorf("Total() = %d, want 0 for an empty census", res.Total())
	}
}
