package observe

import (
	"sort"
	"time"
)

// Observation census (TG-180). TG has no concept of how much of the estate it can actually SEE: every entity
// that produces no alerts reads identically as "fine", whether TG is watching it and it is healthy or TG is
// structurally blind to it. This splits that silence.
//
// The classification is a PROXY, and an honest one. TG stores fired-alert history (ingest_alert: which
// (host, rule) pairs have alarmed and when), NOT alert-RULE definitions — so it cannot know which rules WOULD
// match a currently-silent entity, only which have ever fired for it. So:
//   - observed      — produced a triageable signal within the recency window; TG is watching it, live.
//   - healthy_quiet — silent in the window, but HAS fired before; a rule provably matches it, it is just quiet.
//   - unobservable  — has NEVER produced a triageable signal in the retained history; no rule has ever matched
//                     it, so TG has no evidence it can see this entity at all.
//
// "unobservable" is therefore a LOWER BOUND on true unobservability: a rule that exists but has simply never
// fired reads as unobservable here. That is the safe direction — it over-reports blind spots rather than
// hiding them — and it is the strongest claim TG's own data can support without the external rule catalogue
// (the fault-injection PROBE, TG-180 part 2, is what would make the census falsifiable, and is a separate,
// safety-gated slice).

// CensusState is the closed observation classification of one estate entity.
type CensusState string

const (
	Observed     CensusState = "observed"
	HealthyQuiet CensusState = "healthy_quiet"
	Unobservable CensusState = "unobservable"
)

// CensusStates is the closed set, so a reader (and the coverage metric) enumerates every bucket and a bucket
// with zero members still publishes a 0 rather than vanishing.
var CensusStates = []CensusState{Observed, HealthyQuiet, Unobservable}

// CensusResult is the per-state counts plus the per-entity classification (the reviewable queue TG-180 asks
// for — the actual entities behind each number, not just the totals).
type CensusResult struct {
	Counts   map[CensusState]int
	Entities map[string]CensusState
}

// Census classifies each estate entity by its observation status. `lastFired` maps an entity to when it last
// produced a triageable signal (a missing entry means it has NEVER fired in the retained history); `since`
// bounds the recency window. Entities are deduplicated, and blank names are skipped (a graph can carry an
// empty placeholder). The Counts map always carries every CensusState (0 when empty) so the metric surface is
// stable.
func Census(entities []string, lastFired map[string]time.Time, since time.Time) CensusResult {
	res := CensusResult{
		Counts:   map[CensusState]int{Observed: 0, HealthyQuiet: 0, Unobservable: 0},
		Entities: make(map[string]CensusState, len(entities)),
	}
	for _, e := range entities {
		if e == "" {
			continue
		}
		if _, seen := res.Entities[e]; seen {
			continue // an entity is counted once even if the graph lists it twice
		}
		t, ever := lastFired[e]
		var st CensusState
		switch {
		case ever && !t.Before(since):
			st = Observed // fired at or after the window start
		case ever:
			st = HealthyQuiet // fired, but only before the window
		default:
			st = Unobservable // never fired in the retained history
		}
		res.Entities[e] = st
		res.Counts[st]++
	}
	return res
}

// Total is the number of DISTINCT non-blank entities censused — the denominator every coverage fraction must
// share, so a ratio is never taken against a different population than the numerator counts.
func (r CensusResult) Total() int { return len(r.Entities) }

// HostsInState returns the entities currently classified in a given state, sorted. The fault-injection PROBE
// (TG-180 part 2) draws its candidate population from HostsInState(Unobservable): the entities whose silence is
// a HYPOTHESIS of blindness, which the probe then tests. Sorted so a caller sweeps them deterministically.
func (r CensusResult) HostsInState(st CensusState) []string {
	var out []string
	for e, s := range r.Entities {
		if s == st {
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out
}
