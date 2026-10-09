// Package readertally makes "which actor-evidence reader is actually contributing" a MEASURED fact.
//
// Six evidence readers are armed on this estate (pve, journal, awx, netbox, ldap, tool). The claim carried
// in every recent status review — "5 of 6 evidence readers have returned zero rows all-time" — is a
// per-reader statement, and until now the system could not produce it. The composition root fans a host
// out across every reader and MERGES the results into one slice:
//
//	for _, r := range actorReaders { ev, err := r.Read(...); all = append(all, ev...) }
//
// Each Evidence carries its Domain, so the breakdown was always derivable and never derived. The only
// per-reader signal that existed was the FAILURE log line — so a reader that answered promptly and
// returned nothing was indistinguishable from one carrying the whole result set.
//
// That asymmetry is the defect: a silent reader and a productive reader looked identical, which is exactly
// how a reader stays broken (or stays pointed at hosts nobody triages) for weeks without anyone noticing.
// This tally counts reads, rows, and failures per domain and exposes them on /metrics, so the question is
// answered by an operator surface rather than by a source-code reading.
//
// Deliberately NOT persisted: these are process-lifetime counters like every other /metrics series here.
// A durable store would be a second source of truth about the same fact, and Prometheus already keeps the
// history. Deliberately no error return and no context: an accounting call must never be able to slow or
// fail the evidence path it observes.
package readertally

import (
	"sort"
	"sync"

	"github.com/territory-grounder/grounder/core/metrics"
)

// Metric names, exported so the oracle asserts the same strings the collector emits rather than a copy.
const (
	MetricReads    = "tg_actor_evidence_reads_total"
	MetricRows     = "tg_actor_evidence_rows_total"
	MetricFailures = "tg_actor_evidence_read_failures_total"
)

// Tally counts per-domain reader activity. The zero value is ready and safe for concurrent use.
type Tally struct {
	mu   sync.Mutex
	rows map[string]*counts
}

type counts struct {
	reads    float64
	rows     float64
	failures float64
}

// New returns an empty Tally.
func New() *Tally { return &Tally{rows: map[string]*counts{}} }

func (t *Tally) at(domain string) *counts {
	if t.rows == nil {
		t.rows = map[string]*counts{}
	}
	c := t.rows[domain]
	if c == nil {
		c = &counts{}
		t.rows[domain] = c
	}
	return c
}

// Read records one completed read of domain that returned n evidence rows.
//
// A ZERO-ROW READ IS THE WHOLE POINT and is recorded as such: reads increments, rows does not. A reader
// that is never asked and a reader that is asked and answers nothing are different failures with different
// remedies (wiring vs. scope), and collapsing them is what made this unmeasurable in the first place.
func (t *Tally) Read(domain string, n int) {
	if t == nil || domain == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.at(domain)
	c.reads++
	if n > 0 {
		c.rows += float64(n)
	}
}

// Failed records one read of domain that errored. It is NOT a read: a failure says nothing about whether
// the reader would have had rows, so counting it as a zero-row read would understate every domain that is
// merely unreachable and make an outage look like an empty scope.
func (t *Tally) Failed(domain string) {
	if t == nil || domain == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.at(domain).failures++
}

// Collect renders the tally as /metrics samples, one series per domain per family. Domains are emitted in
// sorted order so the exposition is stable across scrapes.
//
// A domain that has been READ but never produced a row emits rows=0 EXPLICITLY rather than being omitted.
// An absent series and a zero series read identically in a graph and differently in an alert, and the
// zero is the reading this package exists to publish.
func (t *Tally) Collect() []metrics.Sample {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	domains := make([]string, 0, len(t.rows))
	for d := range t.rows {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	out := make([]metrics.Sample, 0, len(domains)*3)
	for _, d := range domains {
		c := t.rows[d]
		lbl := map[string]string{"domain": d}
		out = append(out,
			metrics.Sample{Name: MetricReads, Kind: metrics.Counter, Value: c.reads, Labels: lbl,
				Help: "actor-evidence reads dispatched to this domain reader (a read that returned nothing still counts)"},
			metrics.Sample{Name: MetricRows, Kind: metrics.Counter, Value: c.rows, Labels: map[string]string{"domain": d},
				Help: "actor-evidence rows returned by this domain reader; 0 alongside a non-zero read count means the reader is armed and answering with nothing"},
			metrics.Sample{Name: MetricFailures, Kind: metrics.Counter, Value: c.failures, Labels: map[string]string{"domain": d},
				Help: "actor-evidence reads that ERRORED for this domain (not counted as reads — an unreachable reader is not an empty one)"},
		)
	}
	return out
}

// Summary reports the domains that have been read at least once and returned no rows at all. It is the
// direct answer to "which readers have returned zero rows", for the boot/periodic log line — the metrics
// above are the operator surface, this is the sentence.
func (t *Tally) Summary() (silent, productive []string) {
	if t == nil {
		return nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for d, c := range t.rows {
		if c.reads == 0 {
			continue // never asked — a different fact, and not this function's claim
		}
		if c.rows == 0 {
			silent = append(silent, d)
		} else {
			productive = append(productive, d)
		}
	}
	sort.Strings(silent)
	sort.Strings(productive)
	return silent, productive
}
