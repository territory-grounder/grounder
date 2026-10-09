package tracker

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// MultiHistory reads the incident record from EVERY configured tracker that has one.
//
// It exists because the alternative shape — "use the history capability when exactly one tracker is
// enabled" — is a bug this codebase has already paid for twice on the same day: the notifier seam
// delivered NOTHING when several channels were configured, and tracker history itself was gated on the
// tracker being one specific vendor. A site running ServiceNow for ITSM and YouTrack for engineering
// work has its incident record split across both, and reading one of them is reading half the estate's
// memory.
//
// The success asymmetry matches the notifier fan-out, for the same reason: a source that is down must
// not erase the sources that answered, but ALL sources failing must be an error rather than an empty
// result. "This estate has no record of this host failing" and "the trackers could not be read" are
// different facts, and returning the second as the first would teach the agent that a site with a decade
// of incidents has none.
type MultiHistory struct {
	sources []namedHistory
}

type namedHistory struct {
	name string
	h    History
}

// NewMultiHistory builds a reader over the given sources, keyed by vendor slug for stable ordering.
func NewMultiHistory(sources map[string]History) *MultiHistory {
	out := make([]namedHistory, 0, len(sources))
	for name, h := range sources {
		if h == nil {
			continue // a nil source is not a tracker; counting it would inflate the denominator
		}
		out = append(out, namedHistory{name: name, h: h})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return &MultiHistory{sources: out}
}

// Len reports how many history-capable trackers this reader will query.
func (m *MultiHistory) Len() int {
	if m == nil {
		return 0
	}
	return len(m.sources)
}

// Sources names them, in query order.
func (m *MultiHistory) Sources() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.sources))
	for _, s := range m.sources {
		out = append(out, s.name)
	}
	return out
}

// SearchIncidents queries every source concurrently and returns the merged record, NEWEST FIRST.
//
// The per-source limit is the caller's full limit rather than a share of it: the caller wants the N most
// relevant incidents overall, and pre-dividing the budget would drop a source's best match to make room
// for another source's worst. The merged result is truncated once, at the end, on the merged order.
func (m *MultiHistory) SearchIncidents(ctx context.Context, host, rule string, limit int) ([]HistoricalIncident, error) {
	if m == nil || len(m.sources) == 0 {
		return nil, fmt.Errorf("tracker history: no history-capable tracker is configured")
	}
	type result struct {
		incidents []HistoricalIncident
		err       error
	}
	results := make([]result, len(m.sources))
	var wg sync.WaitGroup
	for i, s := range m.sources {
		wg.Add(1)
		go func(i int, s namedHistory) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					results[i] = result{err: fmt.Errorf("panicked: %v", r)}
				}
			}()
			inc, err := s.h.SearchIncidents(ctx, host, rule, limit)
			for j := range inc {
				inc[j].Source = s.name // stamp provenance; ids are per-vendor namespaces
			}
			results[i] = result{incidents: inc, err: err}
		}(i, s)
	}
	wg.Wait()

	var merged []HistoricalIncident
	var failures []string
	answered := 0
	for i, r := range results {
		if r.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", m.sources[i].name, r.err))
			continue
		}
		answered++
		merged = append(merged, r.incidents...)
	}
	if answered == 0 {
		// EVERY source failed. Returning an empty slice here would be indistinguishable from a site with
		// no incident history at all.
		return nil, fmt.Errorf("tracker history: ALL %d source(s) failed — %s",
			len(m.sources), strings.Join(failures, "; "))
	}

	// Newest first; a zero Filed (unknown, never invented) sorts last rather than pretending to be old or
	// new, with the id as the tie-break so the order cannot reshuffle between identical queries.
	sort.SliceStable(merged, func(i, j int) bool {
		a, b := merged[i], merged[j]
		if a.Filed.IsZero() != b.Filed.IsZero() {
			return !a.Filed.IsZero()
		}
		if !a.Filed.Equal(b.Filed) {
			return a.Filed.After(b.Filed)
		}
		return a.ID < b.ID
	})
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

var _ History = (*MultiHistory)(nil)
