package falsify

import (
	"context"
	"math"
	"sort"
	"time"
)

// THE LEARNED FALSIFIABILITY WINDOW (TG-220, port-fidelity finding #20).
//
// TG used to adjudicate every prediction in one FIXED observation window (TG_FALSIFIABILITY_WINDOW, 10m).
// The predecessor did not: `infragraph.py expected_cascade()` computed
//
//	window = max(DEFAULT_WINDOW_S /* 900 */, int(2 * max_p95))
//
// where max_p95 is the largest p95 OBSERVED propagation delay over the edges the blast-radius traversal
// touched. The consequence of the divergence is a MEASUREMENT bias with a known direction: a cascade that
// takes longer than 10 minutes to manifest is scored before it has happened, so it adjudicates as a MISS in
// TG and a HIT in the predecessor. Because the head-to-head comparison reads exactly these numbers
// (falsifiability, forecast precision/recall), the instrument had to be corrected before the exam is sat.
//
// GRANULARITY — per EDGE, keyed by the ordered HOST PAIR. The predecessor keyed dynamics on rel_id, i.e. one
// row per (source, target, rel_type) edge. TG's estate graph carries the same shape (estate.Edge is
// (From, To, Rel)), but the three surfaces this computation must join across — the estate graph, the durable
// alert ledger (ingest_alert has a host, no rel_type), and the scorer's own input (a target host plus a set of
// predicted hosts) — share exactly ONE key: the ordered (primary → dependent) host pair. That is also the key
// the estate's own self-learning tier already uses (estate.CoOccurrence{Primary, Dependent}), so latency
// learning and confidence learning are keyed identically and a single edge has one identity everywhere.
// Per-rel_type would be strictly coarser AND unobservable from the ledger; per-host would lose the direction
// that makes a propagation delay meaningful.
//
// AGGREGATION — the predecessor took ONE scalar window per prediction (the max over every edge it touched),
// because a prediction is adjudicated as a whole. TG scores the same way (one confusion matrix per
// prediction row), so the port is the same max: a prediction is due only once its SLOWEST claimed edge has
// had time to manifest. Taking anything less would re-introduce the bias on exactly the slow edges the
// finding is about.
//
// DELIBERATE DEVIATIONS from the predecessor (docs/PORT-FIDELITY-AUDIT.md §1.7 — port the logic, not the
// bugs):
//
//   - AN UPPER CAP. The predecessor has none: a single pathological observation (one 6-hour delay sample)
//     yields a 12-hour window for every prediction touching that edge, and the row simply never scores. TG
//     clamps to WindowCap, so the widened window can defer a prediction but can never strand it.
//   - BOTH BOUNDS ARE CONFIG-VISIBLE (TG_FALSIFIABILITY_WINDOW_FLOOR / _MAX). The predecessor's 900 was a
//     hard-coded literal while its siblings were env-tunable.
//   - COMPUTED AT SCORE TIME, not commit time. The predecessor froze window_seconds onto the prediction row;
//     TG has no such column and adding one would be a migration for no gain — the scorer is a poll loop, so
//     re-deriving the window each pass is equivalent for the due/not-due decision and additionally lets the
//     newest observations apply to a prediction still waiting.
//
// FAIL-SAFE DIRECTION. Every unknown resolves to the 900s FLOOR: no latency seam wired, an unreadable
// durable read, an edge with no observations, or a p95 so small that 2×p95 < 900s. The floor is LONGER than
// the 10m fixed window it replaces, so no configuration of this code can manufacture a miss that the
// previous behavior would not also have manufactured. The cap bounds the other end.
//
// Provenance: [F] the predecessor infragraph.py {DEFAULT_WINDOW_S=900, expected_cascade() max(900, 2×p95),
// _percentile() nearest-rank, SAMPLE_CAP=64 trailing samples} re-expressed under the typed spine · [O] INV-22
// (the shuffled-graph control is the tripwire that a wider window is not laundering noise).

// CascadeEdge is the learning key: an ordered host pair meaning "when Primary alerted, Dependent alerted
// after it". It is the same identity estate.CoOccurrence uses for the self-learning confidence tier.
type CascadeEdge struct {
	Primary   string
	Dependent string
}

const (
	// DefaultWindowFloor is the predecessor's DEFAULT_WINDOW_S (900s) — the window when an edge has no
	// observed history, and the lower clamp on every learned window. It is DELIBERATELY longer than the 10m
	// fixed window it replaces: the fail-safe direction for a measurement instrument is to wait longer, never
	// to adjudicate a cascade that has not finished.
	DefaultWindowFloor = 900 * time.Second

	// DefaultWindowCap bounds the learned window so one outlier observation cannot strand a prediction
	// unscored forever (the predecessor has NO cap — see the deviations note above). Two hours is ~8× the
	// floor: wide enough for any cascade the estate has actually shown, short enough that a stuck row is
	// visible within one shift.
	DefaultWindowCap = 2 * time.Hour

	// DefaultLatencyLookback bounds how far back the durable ledger is read for latency evidence. Old
	// topology is not evidence about today's propagation delays.
	DefaultLatencyLookback = 14 * 24 * time.Hour

	// LatencySampleCap is the predecessor's SAMPLE_CAP: only the most recent N observations per edge feed the
	// percentile, so a configuration change shows up within ~N observations instead of being diluted by all
	// history. Applied to the TRAILING end of a chronologically-ordered sample slice (`samples[-CAP:]`).
	LatencySampleCap = 64

	// latencyPercentile / latencyMultiplier are the two halves of the ported rule: 2 × p95.
	latencyPercentile = 0.95
	latencyMultiplier = 2
)

// LatencyReader returns the OBSERVED cascade-latency samples for every edge whose PRIMARY is one of
// primaries, drawn from TG's own durable record of what actually happened (db.CascadeLatencyStore over
// ingest_alert is the production implementation; the oracle drives an in-memory twin). Samples for one edge
// are ordered OLDEST → NEWEST so the trailing SAMPLE_CAP is the most recent evidence.
//
// ok=false means the durable record could NOT be read. It is NOT an error the scorer propagates: every edge
// then falls back to the 900s floor, which is the same fail-safe direction as having no observations at all.
// A monitoring/DB outage must never SHORTEN the window and manufacture misses.
type LatencyReader func(ctx context.Context, primaries []string, since time.Time) (map[CascadeEdge][]time.Duration, bool)

// Percentile is the predecessor's `_percentile`: NEAREST-RANK over the sorted samples at
// idx = clamp(round(pct·(n-1)), 0, n-1), with NO interpolation. Returns ok=false for an empty sample set —
// the "this edge has never been observed" signal that resolves to the floor upstream.
//
// (TG rounds half AWAY FROM ZERO where Python's round() is half-to-even. On an exact half that shifts the
// chosen sample by at most one rank; the ×2 multiplier and the 900s floor dominate any such difference, and
// half-away-from-zero is the definition a reader expects.)
//
// Pure: it copies before sorting, so a caller's slice is never reordered underneath it.
func Percentile(samples []time.Duration, pct float64) (time.Duration, bool) {
	if len(samples) == 0 {
		return 0, false
	}
	s := append([]time.Duration(nil), samples...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(math.Round(pct * float64(len(s)-1)))
	if idx < 0 {
		idx = 0
	}
	if idx > len(s)-1 {
		idx = len(s) - 1
	}
	return s[idx], true
}

// EdgeWindow is the ported rule for ONE edge: max(floor, 2 × p95(observed latency)), clamped to cap.
//
// Only the most recent LatencySampleCap samples count (the predecessor's trailing `samples[-64:]`), so the
// caller must pass them oldest→newest. A negative or zero sample is discarded: a "dependent alerted BEFORE
// the primary" reading is not a propagation delay, and the predecessor's `if p95:` guard likewise ignored a
// zero. No minimum sample count is imposed — the predecessor imposed none either, and it cannot hurt: the
// floor already dominates every p95 below 450s, so a thin sample can only ever widen the window, which is the
// safe direction for a measurement instrument.
func EdgeWindow(samples []time.Duration, floor, cap time.Duration) time.Duration {
	if floor <= 0 {
		floor = DefaultWindowFloor
	}
	if cap < floor {
		cap = floor
	}
	if len(samples) > LatencySampleCap {
		samples = samples[len(samples)-LatencySampleCap:]
	}
	positive := make([]time.Duration, 0, len(samples))
	for _, d := range samples {
		if d > 0 {
			positive = append(positive, d)
		}
	}
	p95, ok := Percentile(positive, latencyPercentile)
	if !ok {
		return floor // never observed — the fail-safe floor, exactly as max(900, 0) resolves
	}
	// int(2 * p95) in the original: truncate to whole seconds so the window is a stable, loggable integer.
	w := (latencyMultiplier * p95).Truncate(time.Second)
	if w < floor {
		w = floor
	}
	if w > cap {
		w = cap
	}
	return w
}

// PredictionWindow is the per-prediction window: the MAX of the per-edge windows over every edge the
// prediction claims — (targetHost → each predicted host) — so a prediction is adjudicated only once its
// SLOWEST claimed cascade has had time to manifest. This is the predecessor's `max_p95` accumulation across
// the traversal, applied one level up (it maxed the percentiles then applied the rule; maxing the per-edge
// WINDOWS is identical because max(floor, 2·x) and the cap clamp are both monotonic in x).
//
// A prediction that claims nothing, or whose every edge is unobserved, gets the floor. The result is always
// within [floor, cap], so no prediction can be deferred indefinitely.
func PredictionWindow(targetHost string, predictedHosts map[string]struct{}, lat map[CascadeEdge][]time.Duration, floor, cap time.Duration) time.Duration {
	if floor <= 0 {
		floor = DefaultWindowFloor
	}
	if cap < floor {
		cap = floor
	}
	w := floor
	// Map iteration order is deliberately unspecified in Go, but max is commutative and associative, so the
	// RESULT does not depend on it — this stays replay-stable, which matters on a scoring path.
	for h := range predictedHosts {
		if ew := EdgeWindow(lat[CascadeEdge{Primary: targetHost, Dependent: h}], floor, cap); ew > w {
			w = ew
		}
	}
	return w
}
