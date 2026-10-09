package main

import (
	"sort"
	"sync"

	"github.com/territory-grounder/grounder/core/metrics"
)

// THE FRONT DOOR COUNTED ONLY WHAT IT LET IN (TG-371).
//
// Measured live 2026-08-07: fifteen tg_ingest_* families publish, and every one of them measures
// ACCEPTANCE or upstream reachability — alerts_total_by_source, last_seen_seconds, recent_total,
// sources_known, upstream_readable, and so on. None counts a refusal.
//
// So three very different situations produced ONE observable — `tg_ingest_source_last_seen_seconds`
// growing:
//
//  1. the source genuinely has nothing to send        (healthy, quiet estate)
//  2. its token rotated and every POST is turned away (broken auth)
//  3. its payload stopped satisfying the grammar      (broken producer, or a version skew)
//
// Only the first is fine, and it is the one that leaves no trace anywhere else either. The rejection
// points were already well-typed with distinct statuses — 503 no registry, 404 unknown source, 400
// unreadable body, 400 payload rejected, 502 triage unavailable — they were simply never tallied.
//
// A COUNTER PER (source, reason), NOT A TOTAL. "Something was refused" does not tell an operator which
// feed went dark or what to fix; the reason distinguishes an auth problem from a grammar problem from a
// TG-side outage, and those are three different people's work.
type ingestRefusals struct {
	mu sync.Mutex
	n  map[[2]string]int64
}

func newIngestRefusals() *ingestRefusals { return &ingestRefusals{n: map[[2]string]int64{}} }

// record tallies one refusal. The reason comes from the handler's CLOSED vocabulary, never from error
// text — a metric label built from free input lets a caller choose this series' cardinality.
func (c *ingestRefusals) record(sourceType, reason string) {
	if c == nil {
		return
	}
	if sourceType == "" {
		// A refusal so early that the URL carried no source is still a refusal, and hiding it would
		// reintroduce the silence this closes at exactly the least-understood rejection point.
		sourceType = "unknown"
	}
	c.mu.Lock()
	c.n[[2]string{sourceType, reason}]++
	c.mu.Unlock()
}

// samples renders the tally. Deliberately emits NOTHING when no refusal has occurred: unlike a gauge,
// an absent counter and a zero counter mean the same thing here, and inventing a zero series per
// (source × reason) would multiply out the whole vocabulary against every source that ever registered.
// The presence of ANY series is itself the signal.
func (c *ingestRefusals) samples() []metrics.Sample {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	keys := make([][2]string, 0, len(c.n))
	for k := range c.n {
		keys = append(keys, k)
	}
	vals := make(map[[2]string]int64, len(c.n))
	for k, v := range c.n {
		vals[k] = v
	}
	c.mu.Unlock()

	// Sorted for a byte-stable scrape.
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	out := make([]metrics.Sample, 0, len(keys))
	for _, k := range keys {
		out = append(out, metrics.Sample{
			Name: "tg_ingest_refused_total", Kind: metrics.Counter,
			Help: "deliveries the ingest front door TURNED AWAY, by source and reason. The front door " +
				"published fifteen families measuring what it accepted and none measuring this, so a " +
				"rotated token, a broken payload grammar and a genuinely quiet source were one reading. " +
				"Any series here means a source is trying and failing — which last_seen_seconds cannot say.",
			Value:  float64(vals[k]),
			Labels: map[string]string{"source_type": k[0], "reason": k[1]},
		})
	}
	return out
}
