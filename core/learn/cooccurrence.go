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

// DefaultRecoveryWindow bounds how long after a host's ONSET its clear/recovery transition may arrive and
// still be attributed to that episode — parity with the chaos tier's recovery correlation bound
// (core/db.DefaultChaosRecoveryWindow, "6 hours"): the same reviewed A6b heal-correlation window, restated
// here because core/learn must not import core/db. An onset with no clear inside this window is dropped
// unattributed rather than paired with a stale, unrelated recovery.
const DefaultRecoveryWindow = 6 * time.Hour

// AlertObservation is one alert seen on the stream: a host alerting at a time. It carries NO remediation —
// observation alone feeds the learner.
type AlertObservation struct {
	Host string
	At   time.Time
}

// ClearObservation is one recovery/clear transition seen on the stream (ingest_transition kind='recovery'):
// a host recovering at a time. Paired with the host's recorded ONSET it yields one observed time-to-recover
// (TG-188: the learned tier's organic MTTR feed — the chaos tier was the sole RecoverySeconds producer).
type ClearObservation struct {
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
	counts map[pairKey]float64
	// delaySum is the running sum of propagation gaps (seconds) per pair, accumulated ONE gap per pair
	// increment so counts[pair] is exactly its denominator. It decays by the same factor as counts (so the
	// MEAN is preserved across a half-life) and is pruned in lockstep (so a pair leaving the tier takes its
	// delay with it). TG-188.
	delaySum  map[pairKey]float64
	trials    map[string]float64 // per-host total observations — the denominator for the base-rate-aware confidence
	lastDecay time.Time          // the previous Decay checkpoint — the "as of" the next half-life is measured from
	// onset→clear pairing state (TG-188 organic recovery learning). onsets holds each host's CURRENT open
	// episode's FIRST alert time (first-raise wins, matching the chaos reader's "first observed recovery
	// at/after its cascade alert" semantics from the other side). It is EPHEMERAL like `recent` — excluded
	// from Snapshot/Restore, so an episode straddling a restart goes unattributed rather than mispaired.
	// recoverySum/recoveryCount accrue per HOST (the learner cannot know which root caused an episode, so
	// recovery is attributed to the recovering host, not to a pair) and decay in lockstep so the mean is
	// preserved — the same discipline as delaySum/counts.
	onsets        map[string]time.Time
	recoverySum   map[string]float64
	recoveryCount map[string]float64
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
	l := &CoOccurrenceLearner{window: window, maxRecent: defaultMaxRecent, counts: map[pairKey]float64{}, delaySum: map[pairKey]float64{}, trials: map[string]float64{}, onsets: map[string]time.Time{}, recoverySum: map[string]float64{}, recoveryCount: map[string]float64{}}
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
		pk := pairKey{a.Host, host}
		l.counts[pk]++
		l.delaySum[pk] += obs.At.Sub(a.At).Seconds() // the propagation gap, one per counted pair (TG-188)
	}
	l.trials[host]++ // every observation of a host is one more incident it could be the root of
	l.recent = append(l.recent, AlertObservation{Host: host, At: obs.At})
	if len(l.recent) > l.maxRecent {
		l.recent = l.recent[len(l.recent)-l.maxRecent:]
	}
	// Onset bookkeeping (TG-188 organic recovery): the FIRST alert of an episode is the onset; a re-fire
	// during the same open episode does not move it. A stale onset that never cleared inside the recovery
	// window is dropped (unattributable) so the map stays bounded by live episodes, not history.
	if cur, ok := l.onsets[host]; !ok || obs.At.Sub(cur) > DefaultRecoveryWindow {
		l.onsets[host] = obs.At
	}
	for h, at := range l.onsets {
		if obs.At.Sub(at) > DefaultRecoveryWindow {
			delete(l.onsets, h)
		}
	}
}

// ObserveClear records one recovery/clear transition (TG-188): paired with the host's recorded onset it
// yields ONE observed time-to-recover, attributed to the HOST (the learner cannot know which root caused an
// episode). A clear for a host with no recorded onset — never alerted since boot, or its episode aged past
// the recovery window — is a no-op: an unattributable recovery teaches nothing, and fabricating a pairing is
// exactly the wrong-reference-set mistake. A clear at/before the onset (clock skew, replayed transition) is
// likewise dropped; a genuine measured recovery is always > 0. Deterministic like Observe: every timestamp
// comes from the observation, never a wall clock.
func (l *CoOccurrenceLearner) ObserveClear(obs ClearObservation) {
	host := strings.TrimSpace(obs.Host)
	if host == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	onset, ok := l.onsets[host]
	if !ok || !obs.At.After(onset) {
		return
	}
	delete(l.onsets, host) // the episode is closed either way
	gap := obs.At.Sub(onset)
	if gap > DefaultRecoveryWindow {
		return // too late to attribute — the same bound the chaos tier's recovery correlation uses
	}
	l.recoverySum[host] += gap.Seconds()
	l.recoveryCount[host]++
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
		mean := 0.0
		if c > 0 {
			mean = l.delaySum[k] / c // decay-invariant: numerator and denominator scaled by the same factor
		}
		// The dependent's observed mean time-to-recover (TG-188): HOST-level attribution applied to every
		// pair it depends in — 0 when never observed (absent-is-not-zero, same as the delay).
		rec := 0.0
		if rc := l.recoveryCount[k.dependent]; rc > 0 {
			rec = l.recoverySum[k.dependent] / rc
		}
		out = append(out, estate.CoOccurrence{Primary: k.primary, Dependent: k.dependent, Count: count, PrimaryTrials: int(math.Round(l.trials[k.primary])), MeanDelaySeconds: mean, MeanRecoverySeconds: rec})
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
			delete(l.counts, k)   // aged out of the self-learning tier
			delete(l.delaySum, k) // its delay estimate leaves with it (TG-188)
			st.Pruned++
			continue
		}
		l.counts[k] = nc
		l.delaySum[k] *= factor // lockstep, so delaySum/counts (the mean) is unchanged by decay
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
	// Recovery evidence ages exactly like the rest: sum and count decay by the SAME factor (the MEAN is
	// preserved across a half-life), and a host whose recovery count falls below one whole observation is
	// dropped — stale MTTR evidence cannot outlive the incidents that produced it (TG-188).
	for h, rc := range l.recoveryCount {
		nrc := rc * factor
		if nrc < countDecayFloor {
			delete(l.recoveryCount, h)
			delete(l.recoverySum, h)
			continue
		}
		l.recoveryCount[h] = nrc
		l.recoverySum[h] *= factor
	}
	return st
}

// DecayOnDisproof shrinks the co-occurrence evidence for the pairs a captured misprediction CONTRADICTS, at
// the SOURCE — the very counts the estate refresh rebuilds its learned edges from — so a disproof survives the
// 5-minute rebuild instead of being overwritten by it (TG-388). It is the targeted, evidence-based counterpart
// to the time-based half-life Decay: a disproved dependency loses a FACTOR of its accumulated count per pass,
// and once that falls below one whole observation (countDecayFloor) the pair is dropped — the age-out the
// estate-graph's confidence decay (floored at 0, unreachable by halving 0.75) could never reach.
//
// It mirrors estate.Graph.DecayOnDisproof's edge selection: for each path (Target, Surprised) it decays the
// pair BOTH ways (an edge implicating a pair points either direction). Only pairs the learner ACTUALLY holds
// are touched — ground-truth (PVE/CMDB) edges are re-seeded from reality on every refresh and carry no
// co-occurrence count here, so they are structurally immune. counts and delaySum decay in LOCKSTEP so the
// propagation-delay MEAN (TG-188) is preserved; trials are per-host denominators SHARED across a host's pairs
// and are deliberately left UNTOUCHED, so disproving one dependent does not distort the base-rate of its
// siblings. factor is clamped to (0,1) — a decay only ever REDUCES; an empty path set is a no-op. Map
// iteration order does not affect the result (each pair decays independently), so this stays replay-stable.
//
// The scoped (Paths) form only: the flat estate.Disproof.Hosts fallback is a graph-side legacy the sole caller
// (the reconciliation pass) never uses, and decaying "every pair touching a disproved host" is exactly the
// over-broad behaviour TG-206 narrowed away.
func (l *CoOccurrenceLearner) DecayOnDisproof(paths []estate.DisproofPath, factor float64) DecayStats {
	if factor <= 0 || factor >= 1 {
		factor = estate.DefaultDecayFactor // a decay can only ever reduce; parity with estate.DecayOnDisproof
	}
	pairs := make(map[pairKey]struct{})
	for _, p := range paths {
		t := strings.TrimSpace(p.Target)
		if t == "" {
			continue
		}
		for _, h := range p.Surprised {
			h = strings.TrimSpace(h)
			if h == "" || h == t {
				continue
			}
			pairs[pairKey{t, h}] = struct{}{}
			pairs[pairKey{h, t}] = struct{}{} // an edge implicating the pair points either way
		}
	}
	if len(pairs) == 0 {
		return DecayStats{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var st DecayStats
	for pk := range pairs {
		c, ok := l.counts[pk]
		if !ok {
			continue // never observed — ground truth and unseen pairs are immune
		}
		nc := c * factor
		if nc < countDecayFloor {
			delete(l.counts, pk)
			delete(l.delaySum, pk)
			st.Pruned++
			continue
		}
		l.counts[pk] = nc
		l.delaySum[pk] *= factor // lockstep, so the propagation-delay mean is unchanged by decay
		st.Pairs++
	}
	return st
}

// PairObservation is one co-occurrence pair with its RAW (unrounded) decay-state weights, for durable
// persistence (TG-388 face c). The CoOccurrences() view rounds count to a whole observation and collapses
// delaySum into a mean — lossy; a faithful restore needs the floats.
type PairObservation struct {
	Primary   string
	Dependent string
	Count     float64
	DelaySum  float64
}

// HostTrials is one host's raw trial denominator — the base-rate denominator for every pair it roots.
type HostTrials struct {
	Host   string
	Trials float64
}

// HostRecovery is one host's raw observed-recovery evidence (TG-188): the decayable sum of time-to-recover
// seconds and its observation count, as floats for a faithful restore (the mean is Sum/Count).
type HostRecovery struct {
	Host  string
	Sum   float64
	Count float64
}

// Snapshot is the learner's DURABLE state: the co-occurrence counts + delay sums, the per-host trial
// denominators, and the per-host recovery evidence, as raw floats. The `recent` working set is excluded (an
// ephemeral within-window buffer the live stream refills), and so are `onsets` (an episode straddling a
// restart goes unattributed rather than mispaired) and the lastDecay checkpoint (it re-baselines on the
// first post-restore Decay; the half-life clock simply does not count downtime — negligible against a
// 30-day half-life).
type Snapshot struct {
	Pairs      []PairObservation
	Trials     []HostTrials
	Recoveries []HostRecovery
}

// Snapshot returns the learner's durable state under lock, deterministically ordered so a save is stable
// (byte-identical when nothing has changed).
func (l *CoOccurrenceLearner) Snapshot() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	snap := Snapshot{Pairs: make([]PairObservation, 0, len(l.counts)), Trials: make([]HostTrials, 0, len(l.trials))}
	for k, c := range l.counts {
		snap.Pairs = append(snap.Pairs, PairObservation{Primary: k.primary, Dependent: k.dependent, Count: c, DelaySum: l.delaySum[k]})
	}
	for h, tr := range l.trials {
		snap.Trials = append(snap.Trials, HostTrials{Host: h, Trials: tr})
	}
	for h, rc := range l.recoveryCount {
		snap.Recoveries = append(snap.Recoveries, HostRecovery{Host: h, Sum: l.recoverySum[h], Count: rc})
	}
	sort.Slice(snap.Pairs, func(i, j int) bool {
		if snap.Pairs[i].Primary != snap.Pairs[j].Primary {
			return snap.Pairs[i].Primary < snap.Pairs[j].Primary
		}
		return snap.Pairs[i].Dependent < snap.Pairs[j].Dependent
	})
	sort.Slice(snap.Trials, func(i, j int) bool { return snap.Trials[i].Host < snap.Trials[j].Host })
	sort.Slice(snap.Recoveries, func(i, j int) bool { return snap.Recoveries[i].Host < snap.Recoveries[j].Host })
	return snap
}

// Restore REPLACES the learner's counts, delay sums, and trials with a persisted snapshot (load-on-boot,
// TG-388 face c). It must run BEFORE the ingest feed starts — there is no merge, so a concurrent Observe would
// race a half-loaded map. An empty snapshot leaves an empty learner (a first boot / empty DB). Entries with a
// blank/self endpoint or a non-positive count are skipped defensively, so a corrupt row can never seed a
// phantom dependency. `recent` stays empty and `lastDecay` stays zero (the next Decay re-baselines).
func (l *CoOccurrenceLearner) Restore(snap Snapshot) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counts = make(map[pairKey]float64, len(snap.Pairs))
	l.delaySum = make(map[pairKey]float64, len(snap.Pairs))
	for _, p := range snap.Pairs {
		primary, dependent := strings.TrimSpace(p.Primary), strings.TrimSpace(p.Dependent)
		if primary == "" || dependent == "" || primary == dependent || p.Count <= 0 {
			continue
		}
		pk := pairKey{primary, dependent}
		l.counts[pk] = p.Count
		if p.DelaySum > 0 {
			l.delaySum[pk] = p.DelaySum
		}
	}
	l.trials = make(map[string]float64, len(snap.Trials))
	for _, ht := range snap.Trials {
		host := strings.TrimSpace(ht.Host)
		if host == "" || ht.Trials <= 0 {
			continue
		}
		l.trials[host] = ht.Trials
	}
	l.recoverySum = make(map[string]float64, len(snap.Recoveries))
	l.recoveryCount = make(map[string]float64, len(snap.Recoveries))
	for _, hr := range snap.Recoveries {
		host := strings.TrimSpace(hr.Host)
		if host == "" || hr.Count <= 0 || hr.Sum <= 0 {
			continue // a corrupt row can never seed a phantom recovery estimate
		}
		l.recoverySum[host] = hr.Sum
		l.recoveryCount[host] = hr.Count
	}
	l.onsets = map[string]time.Time{} // ephemeral: an episode straddling a restart goes unattributed
}
