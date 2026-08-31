package main

import (
	"sort"
	"sync"

	"github.com/territory-grounder/grounder/core/metrics"
)

// ingestPredrop tallies alerts the ingest front door accepted BUT did not turn into a new triage session —
// the pre-admission drops TG-380 says are permanently unknowable today. Distinct from ingestRefusals (which
// counts deliveries TURNED AWAY at the door with a non-2xx): a predrop is a 2xx-accepted alert that
// nonetheless mints no new work, so "the upstream sent more than we triaged" is invisible without it. The
// pve03 postmortem could not answer "if the upstreams sent more than 157 notifications, how many did we
// collapse?" precisely because nothing counts this.
//
// The reason vocabulary is CLOSED (ClampPredropReason) — a label built from anything caller-derived would
// let cardinality explode; these are the two front-door drop paths, both grounder-internal decisions.
type ingestPredrop struct {
	mu sync.Mutex
	n  map[string]int64
}

func newIngestPredrop() *ingestPredrop { return &ingestPredrop{n: map[string]int64{}} }

// The CLOSED predrop-reason set.
const (
	predropRejectDuplicate    = "reject_duplicate"    // a re-fire of an incident whose triage workflow is still in flight — StartTriage returns the existing id, no new session
	predropRecoveryTransition = "recovery_transition" // a provider RECOVERY transition — captured as clear-evidence, mints no triage (ingest.go)
)

var predropReasonSet = map[string]bool{predropRejectDuplicate: true, predropRecoveryTransition: true}

// clampPredropReason folds an unrecognized reason to "other" rather than mint an unbounded series.
func clampPredropReason(reason string) string {
	if predropReasonSet[reason] {
		return reason
	}
	return "other"
}

// record tallies one pre-admission drop by reason. A nil receiver is a no-op.
func (c *ingestPredrop) record(reason string) {
	if c == nil {
		return
	}
	r := clampPredropReason(reason)
	c.mu.Lock()
	c.n[r]++
	c.mu.Unlock()
}

// samples renders the tally. Like ingestRefusals it emits NOTHING when no drop has occurred — an absent
// counter and a zero counter mean the same thing, and the presence of any series is itself the signal.
func (c *ingestPredrop) samples() []metrics.Sample {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	reasons := make([]string, 0, len(c.n))
	vals := make(map[string]int64, len(c.n))
	for k, v := range c.n {
		reasons = append(reasons, k)
		vals[k] = v
	}
	c.mu.Unlock()
	sort.Strings(reasons)
	out := make([]metrics.Sample, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, metrics.Sample{
			Name: "tg_ingest_predrop_total", Kind: metrics.Counter,
			Help: "alerts the front door ACCEPTED (2xx) but did NOT turn into a new triage session, by " +
				"reason (reject_duplicate: a re-fire of an in-flight incident; recovery_transition: a " +
				"provider recovery). The denominator for 'the upstream sent more than we triaged' — the " +
				"pre-admission drop the pve03 cascade could not measure (TG-380).",
			Value:  float64(vals[r]),
			Labels: map[string]string{"reason": r},
		})
	}
	return out
}
