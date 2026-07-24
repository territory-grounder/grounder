// Package learn is Territory Grounder's outcome-labelled memory: it turns the OBSERVED alert stream into the
// estate's self-learning tier without any mutation. When hosts alert together inside a cascade window, the
// earlier (root) host and the later (consequent) host form a candidate dependency; repeated observation
// raises a learned edge's confidence (hard-capped 0.75, so it only ever ENRICHES prediction). This is the
// "outcome-labelled memory" dimension realized in read-only mode — no action is required, only observation.
//
// Determinism: the learner takes every timestamp from the observation itself (never a wall clock), so a
// replay of the same alert stream yields the same counts — safe inside deterministic workflow code and
// reproducible under test.
package learn

import (
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
)

// DefaultCascadeWindow is how close in time two alerts must be to be treated as co-occurring — the same
// window the suppression/prediction lanes use for a cascade.
const DefaultCascadeWindow = 10 * time.Minute

// defaultMaxRecent bounds the in-window working set so a pathological alert storm cannot grow memory or make
// pairing quadratic without limit.
const defaultMaxRecent = 512

// countDecayFloor is the whole-observation floor below which a half-life-decayed co-occurrence count is
// dropped: a count under 0.5 rounds to zero and can never reach the learned-edge threshold again without
// FRESH evidence, so keeping it only leaks memory. Dropping it is how a co-occurrence that stopped recurring
// finally ages out of the self-learning tier (spec/018, Gulli ch14).
const countDecayFloor = 0.5

// AlertObservation is one alert seen on the stream: a host alerting at a time. It carries NO remediation —
// observation alone feeds the learner.
type AlertObservation struct {
	Host string
	At   time.Time
}

type pairKey struct{ primary, dependent string }

// CoOccurrenceLearner accumulates incident co-occurrence counts from observed alerts. Within the cascade
// window, when host A alerted before host B, it records (A → B): B's alert may be a consequence of A's, so
// B depends on A. Counts accrue across all observations; a pair must be seen repeatedly before the learned
// tier promotes it to an edge (estate.LearnedMinObservations), so a single coincidence never becomes a
// dependency.
type CoOccurrenceLearner struct {
	mu        sync.Mutex // the learner is shared between the ingest feed and the estate refresh — guard state
	window    time.Duration
	maxRecent int
	recent    []AlertObservation // recent alerts within the window, in arrival (≈ chronological) order
	// counts/trials are FLOATS so the recency half-life (Decay) can shrink them continuously toward zero; an
	// Observe still adds a whole 1.0, so recent evidence keeps its full weight while old evidence fades.
	counts    map[pairKey]float64
	trials    map[string]float64 // per-host total observations — the denominator for the base-rate-aware confidence
	lastDecay time.Time          // the previous Decay checkpoint — the "as of" the next half-life is measured from
}

// Option configures a CoOccurrenceLearner.
type Option func(*CoOccurrenceLearner)

// WithMaxRecent overrides the in-window working-set cap.
func WithMaxRecent(n int) Option {
	return func(l *CoOccurrenceLearner) {
		if n > 0 {
			l.maxRecent = n
		}
	}
}

// NewCoOccurrenceLearner builds a learner for a cascade window (<=0 uses the default).
func NewCoOccurrenceLearner(window time.Duration, opts ...Option) *CoOccurrenceLearner {
	if window <= 0 {
		window = DefaultCascadeWindow
	}
	l := &CoOccurrenceLearner{window: window, maxRecent: defaultMaxRecent, counts: map[pairKey]float64{}, trials: map[string]float64{}}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Observe records one alert. It first evicts recent alerts older than the window (relative to this alert's
// time), then pairs the new alert with each still-recent EARLIER alert on a DIFFERENT host — the earlier host
// is the root, the new host the consequent — and finally appends it to the working set.
func (l *CoOccurrenceLearner) Observe(obs AlertObservation) {
	host := strings.TrimSpace(obs.Host)
	if host == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := obs.At.Add(-l.window)
	drop := 0
	for drop < len(l.recent) && l.recent[drop].At.Before(cutoff) {
		drop++
	}
	l.recent = l.recent[drop:]
	for _, a := range l.recent {
		if a.Host == host || a.At.After(obs.At) {
			continue // same host, or not actually earlier — not a root→consequent pair
		}
		l.counts[pairKey{a.Host, host}]++
	}
	l.trials[host]++ // every observation of a host is one more incident it could be the root of
	l.recent = append(l.recent, AlertObservation{Host: host, At: obs.At})
	if len(l.recent) > l.maxRecent {
		l.recent = l.recent[len(l.recent)-l.maxRecent:]
	}
}

// CoOccurrences returns the accumulated observations as estate co-occurrence rows, deterministically ordered
// (by descending count, then primary, then dependent) so a caller/snapshot is stable.
func (l *CoOccurrenceLearner) CoOccurrences() []estate.CoOccurrence {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]estate.CoOccurrence, 0, len(l.counts))
	for k, c := range l.counts {
		count := int(math.Round(c))
		if count <= 0 {
			continue // decayed below a whole observation — no evidentiary weight, contributes no edge
		}
		out = append(out, estate.CoOccurrence{Primary: k.primary, Dependent: k.dependent, Count: count, PrimaryTrials: int(math.Round(l.trials[k.primary]))})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Primary != out[j].Primary {
			return out[i].Primary < out[j].Primary
		}
		return out[i].Dependent < out[j].Dependent
	})
	return out
}

// LearnedSource snapshots the current counts into an estate.LearnedSource ready to seed the graph — the
// bridge from observed outcomes to the self-learning estate tier.
func (l *CoOccurrenceLearner) LearnedSource() *estate.LearnedSource {
	return estate.NewLearnedSource(l.CoOccurrences())
}

// DecayStats reports one half-life decay pass.
type DecayStats struct {
	Pairs  int // distinct co-occurrence pairs whose count survived and was reduced
	Pruned int // pairs whose count decayed below one whole observation and were dropped
}

// Decay applies an exponential HALF-LIFE to every accumulated co-occurrence count and per-host trial count,
// so OLD evidence fades and RECENT observations dominate the self-learning tier — the periodic reconciliation
// (spec/018, Gulli ch14) that stops a co-occurrence which stopped recurring from over-weighting the estate
// graph forever. Every count is multiplied by 2^(-elapsed/halfLife), where elapsed is measured from the
// previous Decay checkpoint; the FIRST call only SETS the checkpoint (there is no prior interval to decay
// over). A pair whose count falls below one whole observation (countDecayFloor) is dropped — it can no longer
// reach the learned-edge threshold without fresh evidence. A non-positive halfLife, a zero/backwards elapsed,
// or a clock that did not advance is a no-op. Because both counts AND trials decay by the SAME factor, the
// base-rate ratio (Count/PrimaryTrials) is preserved — decay shrinks the SAMPLE SIZE (evidence strength),
// never the learned confidence's shape.
//
// Decay is a MAINTENANCE operation driven by an EXPLICIT wall clock passed by the caller — deliberately
// separate from the deterministic, replay-safe Observe path (which takes every timestamp from the
// observation and never reads a clock). It is safe to call concurrently with Observe / CoOccurrences.
func (l *CoOccurrenceLearner) Decay(now time.Time, halfLife time.Duration) DecayStats {
	if halfLife <= 0 {
		return DecayStats{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastDecay.IsZero() {
		l.lastDecay = now // establish the baseline; the next call decays over the interval since here
		return DecayStats{}
	}
	elapsed := now.Sub(l.lastDecay)
	if elapsed <= 0 {
		return DecayStats{} // clock did not advance (or went backwards) — nothing to age
	}
	factor := math.Exp2(-elapsed.Seconds() / halfLife.Seconds())
	l.lastDecay = now
	var st DecayStats
	for k, c := range l.counts {
		nc := c * factor
		if nc < countDecayFloor {
			delete(l.counts, k) // aged out of the self-learning tier
			st.Pruned++
			continue
		}
		l.counts[k] = nc
		st.Pairs++
	}
	for h, t := range l.trials {
		nt := t * factor
		if nt < countDecayFloor {
			delete(l.trials, h)
			continue
		}
		l.trials[h] = nt
	}
	return st
}
