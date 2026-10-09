package falsify

import (
	"testing"
	"time"
)

// Percentile is the predecessor's `_percentile` — NEAREST-RANK on the sorted samples, no interpolation.
// These cases pin the index arithmetic, because the whole window rule hangs off it.
func TestPercentileIsNearestRankWithNoInterpolation(t *testing.T) {
	s := func(secs ...int) []time.Duration {
		out := make([]time.Duration, 0, len(secs))
		for _, n := range secs {
			out = append(out, time.Duration(n)*time.Second)
		}
		return out
	}
	cases := []struct {
		name    string
		samples []time.Duration
		pct     float64
		want    time.Duration
		wantOK  bool
	}{
		{"no samples is the never-observed signal", nil, 0.95, 0, false},
		{"a single sample is its own p95", s(42), 0.95, 42 * time.Second, true},
		// n=20 ⇒ idx = round(0.95*19) = round(18.05) = 18 ⇒ the 19th smallest.
		{"p95 of 1..20 picks the 19th smallest", s(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20), 0.95, 19 * time.Second, true},
		// The result is a REAL sample, never an interpolated value between two of them.
		{"p95 never interpolates between samples", s(10, 1000), 0.95, 1000 * time.Second, true},
		{"p50 of an odd set is the middle sample", s(5, 100, 7), 0.50, 7 * time.Second, true},
		{"input order does not matter", s(900, 30, 60, 45), 0.95, 900 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Percentile(tc.samples, tc.pct)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("Percentile(%v, %v) = (%s, %v), want (%s, %v)", tc.samples, tc.pct, got, ok, tc.want, tc.wantOK)
			}
		})
	}
	// Purity: the caller's slice must not be reordered underneath it.
	in := s(900, 30, 60)
	_, _ = Percentile(in, 0.95)
	if in[0] != 900*time.Second {
		t.Fatalf("Percentile reordered its caller's slice: %v", in)
	}
}

// THE PORTED RULE, per edge: window = max(900s, 2 x p95), clamped to the cap. Each row is a claim about a
// direction the measurement can be wrong in.
func TestEdgeWindowPortsMax900Times2P95(t *testing.T) {
	secs := func(n ...int) []time.Duration {
		out := make([]time.Duration, 0, len(n))
		for _, v := range n {
			out = append(out, time.Duration(v)*time.Second)
		}
		return out
	}
	const floor, cap = DefaultWindowFloor, DefaultWindowCap
	cases := []struct {
		name    string
		samples []time.Duration
		want    time.Duration
	}{
		{"an edge never observed gets the 900s floor — the fail-safe, exactly as max(900, 0)", nil, 900 * time.Second},
		{"a FAST edge keeps a tight window: 2x30s is far under the floor", secs(28, 30, 31, 29, 30), 900 * time.Second},
		{"2x p95 exactly at the floor stays at the floor", secs(450), 900 * time.Second},
		{"a SLOW edge widens: p95=900s (a 15-minute cascade) ⇒ 1800s", secs(880, 890, 900), 1800 * time.Second},
		{"one slow sample among fast ones is what p95 is FOR — it widens", secs(30, 30, 30, 30, 30, 30, 30, 30, 30, 1200), 2400 * time.Second},
		{"a pathological outlier is CAPPED, never unbounded (the predecessor had no cap)", secs(6 * 60 * 60), cap},
		{"a non-positive reading is not a propagation delay and is discarded", secs(0, -5), 900 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EdgeWindow(tc.samples, floor, cap); got != tc.want {
				t.Fatalf("EdgeWindow = %s, want %s", got, tc.want)
			}
		})
	}
}

// The trailing SAMPLE_CAP: only the most recent LatencySampleCap observations count, so a stale era of slow
// propagation cannot hold the window open forever after the estate got fast. Ported from `samples[-64:]`.
func TestEdgeWindowUsesOnlyTheMostRecentSampleCapObservations(t *testing.T) {
	var samples []time.Duration
	for i := 0; i < LatencySampleCap; i++ { // the OLD era: uniformly slow
		samples = append(samples, 1200*time.Second)
	}
	for i := 0; i < LatencySampleCap; i++ { // the RECENT era: uniformly fast
		samples = append(samples, 20*time.Second)
	}
	if got := EdgeWindow(samples, DefaultWindowFloor, DefaultWindowCap); got != DefaultWindowFloor {
		t.Fatalf("window = %s, want the floor — the slow era is off the trailing %d-sample tail", got, LatencySampleCap)
	}
	// MUTATION CONTROL for the cap: without the trailing trim the old era survives into the percentile and the
	// window stays wide. Pinning the untrimmed value proves the trim is what decides the assertion above.
	if got := EdgeWindow(samples[:LatencySampleCap+1], DefaultWindowFloor, DefaultWindowCap); got != 2400*time.Second {
		t.Fatalf("untrimmed window = %s, want 2400s — if this is also the floor the test above proves nothing", got)
	}
}

// A prediction is adjudicated as a whole, so its window is the MAX over the edges it claims: it is due only
// once its SLOWEST claimed cascade has had time to manifest. (The predecessor maxed p95 across the traversal
// and applied the rule once; maxing the per-edge windows is identical — both halves are monotonic in p95.)
func TestPredictionWindowTakesTheSlowestClaimedEdge(t *testing.T) {
	lat := map[CascadeEdge][]time.Duration{
		{Primary: "pve01", Dependent: "fast01"}: {20 * time.Second, 22 * time.Second},
		{Primary: "pve01", Dependent: "slow01"}: {900 * time.Second},
		{Primary: "other", Dependent: "slow01"}: {3000 * time.Second}, // a DIFFERENT primary — must not leak in
	}
	floor, cap := DefaultWindowFloor, DefaultWindowCap
	if got := PredictionWindow("pve01", map[string]struct{}{"fast01": {}}, lat, floor, cap); got != floor {
		t.Fatalf("a fast-only prediction must keep a tight window, got %s", got)
	}
	if got := PredictionWindow("pve01", map[string]struct{}{"fast01": {}, "slow01": {}}, lat, floor, cap); got != 1800*time.Second {
		t.Fatalf("the slowest claimed edge sets the window, got %s want 1800s", got)
	}
	if got := PredictionWindow("pve01", map[string]struct{}{"unseen01": {}}, lat, floor, cap); got != floor {
		t.Fatalf("an unobserved edge falls back to the floor, got %s", got)
	}
	if got := PredictionWindow("pve01", nil, lat, floor, cap); got != floor {
		t.Fatalf("a prediction claiming nothing gets the floor, got %s", got)
	}
	// The edge key is DIRECTED and keyed on the ordered pair: latency learned for (other → slow01) says nothing
	// about (pve01 → slow01) and must not widen it.
	if got := PredictionWindow("pve01", map[string]struct{}{"slow01": {}}, lat, floor, cap); got != 1800*time.Second {
		t.Fatalf("a foreign primary's latency leaked into this edge: got %s want 1800s", got)
	}
}

// Both bounds hold under misconfiguration: the window is ALWAYS within [floor, cap], so no prediction can be
// deferred indefinitely and none can be adjudicated earlier than the floor.
func TestWindowIsAlwaysWithinItsBounds(t *testing.T) {
	floor, cap := 900*time.Second, 30*time.Minute
	for _, samples := range [][]time.Duration{
		nil, {time.Second}, {900 * time.Second}, {24 * time.Hour}, {0}, {-time.Hour},
	} {
		w := EdgeWindow(samples, floor, cap)
		if w < floor || w > cap {
			t.Fatalf("EdgeWindow(%v) = %s, outside [%s, %s]", samples, w, floor, cap)
		}
	}
	// A cap configured BELOW the floor is a misconfiguration; the floor wins (the clamp may only ever widen).
	if got := EdgeWindow([]time.Duration{9000 * time.Second}, 900*time.Second, time.Minute); got != 900*time.Second {
		t.Fatalf("a sub-floor cap must not shorten below the floor, got %s", got)
	}
}
