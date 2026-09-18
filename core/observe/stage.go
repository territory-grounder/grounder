package observe

import (
	"sort"
	"sync"

	"github.com/territory-grounder/grounder/core/metrics"
)

// StageTally is the TG-380 decision-stage instrument: the thread-safe accumulator behind the
// tg_stage_{offered,eligible,acted}_total{stage} triple. During the 2026-08-06 pve03 cascade the machine's
// decision stages (suppress/correlate/classify/predict/gate/breaker) were invisible in real time — a
// cascade could be autopsied from the DB but never WATCHED — because no metric described a decision. This
// is the standing instrument that closes that: every stage records offered (the denominator), eligible (got
// past the stage's short-circuit to its real logic), and acted (the numerator), so a zero is interpretable
// — "nothing to act on" (eligible=0) is distinct from "dead stage" (eligible>0, acted=0), the exact
// discrimination TG-377 needed and TG-380 generalises.
//
// A stage records ONE observation per decision via Record; the counters are monotonic. offered ≥ eligible ≥
// acted holds by construction (a caller passing acted=true must pass eligible=true — Record enforces it, so
// a wiring bug cannot publish an impossible triple). A nil *StageTally is a no-op (never panics), so an
// unwired deployment is silently observation-free rather than a crash.
type StageTally struct {
	mu sync.Mutex
	// per stage: [offered, eligible, acted]
	counts map[string][3]int64
}

// NewStageTally returns an armed decision-stage tally.
func NewStageTally() *StageTally { return &StageTally{counts: map[string][3]int64{}} }

// Record books one decision for a stage. offered is always incremented (every call is a decision the stage
// saw). eligible/acted are booked when true. It ENFORCES the subset invariant: acted implies eligible — a
// caller that suppressed/acted necessarily got past the short-circuit, so acted=true silently promotes
// eligible=true rather than publishing offered≥acted>eligible, which no reader could make sense of.
func (t *StageTally) Record(stage string, eligible, acted bool) {
	if t == nil {
		return
	}
	if acted {
		eligible = true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.counts == nil {
		t.counts = map[string][3]int64{}
	}
	c := t.counts[stage]
	c[0]++
	if eligible {
		c[1]++
	}
	if acted {
		c[2]++
	}
	t.counts[stage] = c
}

// Snapshot returns the current (offered, eligible, acted) for a stage — for tests and the guard's exercise.
func (t *StageTally) Snapshot(stage string) (offered, eligible, acted int64) {
	if t == nil {
		return 0, 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.counts[stage]
	return c[0], c[1], c[2]
}

// Samples renders the triple for every stage that has seen traffic, in a stable stage order (deterministic
// scrape). All three families are emitted for each such stage — never acted alone — so the denominator is
// always beside the numerator. A stage with zero traffic emits nothing (a counter's absence == zero); the
// producer-scan guard, not a permanent zero series, is what proves a wired stage is live.
func (t *StageTally) Samples() []metrics.Sample {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	stages := make([]string, 0, len(t.counts))
	for s := range t.counts {
		stages = append(stages, s)
	}
	sort.Strings(stages)
	out := make([]metrics.Sample, 0, 3*len(stages))
	for _, s := range stages {
		c := t.counts[s]
		out = append(out,
			metrics.StageOfferedSample(s, float64(c[0])),
			metrics.StageEligibleSample(s, float64(c[1])),
			metrics.StageActedSample(s, float64(c[2])),
		)
	}
	return out
}
