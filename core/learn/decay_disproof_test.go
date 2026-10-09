package learn

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
)

// observePairs accumulates `n` co-occurrences of root->cons starting at base, each an hour apart (far past the
// default 10-minute window) so no cross-incident pairs form WITHIN the call. After it, counts[(root,cons)] == n.
// Callers stacking several pairs on ONE learner MUST pass FAR-APART bases, or the shared `recent` working set
// silently cross-contaminates them — a call at an overlapping time pairs its hosts with the previous call's
// still-recent observations.
func observePairs(l *CoOccurrenceLearner, base time.Time, root, cons string, n int) {
	for i := 0; i < n; i++ {
		at := base.Add(time.Duration(i) * time.Hour)
		l.Observe(AlertObservation{Host: root, At: at})
		l.Observe(AlertObservation{Host: cons, At: at.Add(2 * time.Second)})
	}
}

// fixtureDay0 is a stable base; add multiples of 100 days for pairs that must not cross-contaminate.
var fixtureDay0 = time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)

// learnedEdgeConf reads the confidence of the learned edge dependent->primary (From:dependent To:primary) AS
// THE ESTATE REFRESH WOULD REBUILD IT — through LearnedSource().Edges(), recomputed from the live counts. This
// is the whole point of TG-388: a disproof is real only if it survives this recompute.
func learnedEdgeConf(t *testing.T, l *CoOccurrenceLearner, dependent, primary string) (float64, bool) {
	t.Helper()
	edges, err := l.LearnedSource().Edges(context.Background())
	if err != nil {
		t.Fatalf("LearnedSource().Edges: %v", err)
	}
	for _, e := range edges {
		if e.From.Name == dependent && e.To.Name == primary {
			return e.Confidence, true
		}
	}
	return 0, false
}

// TestDecayOnDisproofPersistsThroughEdgeRecompute is the killing scenario for TG-388 face (a): a disproof
// must lower the learned edge AS RECOMPUTED BY THE REFRESH, not just a transient graph clone the refresh
// overwrites. It also pins the scoping (a control pair is untouched) and the trials invariant (a sibling that
// shares the disproved pair's primary keeps its confidence — trials are not decayed).
func TestDecayOnDisproofPersistsThroughEdgeRecompute(t *testing.T) {
	l := NewCoOccurrenceLearner(0)
	observePairs(l, fixtureDay0, "A", "B", 8)                   // the pair we will disprove
	observePairs(l, fixtureDay0.AddDate(0, 0, 100), "A", "E", 8) // a SIBLING sharing primary A — base-rate must not move
	observePairs(l, fixtureDay0.AddDate(0, 0, 200), "C", "D", 8) // an unrelated control

	abBefore, ok := learnedEdgeConf(t, l, "B", "A")
	if !ok {
		t.Fatalf("A->B learned edge (From:B To:A) missing before disproof — the fixture is wrong")
	}
	aeBefore, ok := learnedEdgeConf(t, l, "E", "A")
	if !ok {
		t.Fatalf("sibling A->E edge missing before disproof")
	}
	cdBefore, ok := learnedEdgeConf(t, l, "D", "C")
	if !ok {
		t.Fatalf("control C->D edge missing before disproof")
	}

	st := l.DecayOnDisproof([]estate.DisproofPath{{Target: "A", Surprised: []string{"B"}}}, 0.5)
	if st.Pairs == 0 && st.Pruned == 0 {
		t.Fatalf("DecayOnDisproof reported no effect (%+v) — it did not touch the disproved pair, so the fix is inert", st)
	}

	// (a) PERSISTENCE: recompute the edge from the (now decayed) counts. A count of 8 halved to 4 is still
	// above the learned-edge threshold, so the edge stays but its confidence MUST have dropped.
	abAfter, still := learnedEdgeConf(t, l, "B", "A")
	if !still {
		t.Fatalf("A->B edge vanished after a single 0.5 disproof of count 8 — expected a drop, not an age-out")
	}
	if abAfter >= abBefore {
		t.Errorf("A->B confidence did not drop through the recompute: before=%.4f after=%.4f — the disproof did NOT persist into the rebuilt edge (TG-388 face a)", abBefore, abAfter)
	}
	// trials untouched: the sibling A->E keeps its exact confidence.
	if aeAfter, ok := learnedEdgeConf(t, l, "E", "A"); !ok || aeAfter != aeBefore {
		t.Errorf("sibling A->E moved (before=%.4f after=%.4f ok=%v) — disproving one dependent must not distort a sibling's base-rate (trials must not decay)", aeBefore, aeAfter, ok)
	}
	// scoping: the unrelated control is untouched.
	if cdAfter, ok := learnedEdgeConf(t, l, "D", "C"); !ok || cdAfter != cdBefore {
		t.Errorf("control C->D moved (before=%.4f after=%.4f ok=%v) — disproof decay must be scoped to the disproved pair", cdBefore, cdAfter, ok)
	}
}

// TestDecayOnDisproofAgesOutBelowFloor is TG-388 face (b): repeated disproof drives a pair's count below one
// whole observation (countDecayFloor) and it is PRUNED — the age-out the estate graph's Floor=0 could never
// reach. count 3, halved three times: 3 -> 1.5 -> 0.75 -> 0.375 (< 0.5) => pruned, gone from the tier.
func TestDecayOnDisproofAgesOutBelowFloor(t *testing.T) {
	l := NewCoOccurrenceLearner(0)
	observePairs(l, fixtureDay0, "A", "B", 3)
	if _, ok := learnedEdgeConf(t, l, "B", "A"); !ok {
		t.Fatalf("A->B edge missing before disproof")
	}
	pruned := false
	for i := 0; i < 3; i++ {
		if st := l.DecayOnDisproof([]estate.DisproofPath{{Target: "A", Surprised: []string{"B"}}}, 0.5); st.Pruned > 0 {
			pruned = true
		}
	}
	if !pruned {
		t.Errorf("A->B was never pruned across 3 disproofs — age-out below countDecayFloor did not fire")
	}
	if _, ok := learnedEdgeConf(t, l, "B", "A"); ok {
		t.Errorf("A->B edge still present after aging out — a pruned pair must contribute no edge")
	}
	for _, co := range l.CoOccurrences() {
		if co.Primary == "A" && co.Dependent == "B" {
			t.Errorf("pair A->B still in CoOccurrences() after prune: %+v", co)
		}
	}
}

// TestDecayOnDisproofEmptyIsNoop — the empty-input killing mutation (TG-365): no paths, an empty target, and
// an empty/self surprise host must all be exact no-ops (no panic, no state change), so a caller with nothing
// to disprove never silently ages the tier.
func TestDecayOnDisproofEmptyIsNoop(t *testing.T) {
	l := NewCoOccurrenceLearner(0)
	observePairs(l, fixtureDay0, "A", "B", 5)
	before := l.CoOccurrences()

	for _, paths := range [][]estate.DisproofPath{
		nil,
		{},
		{{Target: "", Surprised: []string{"B"}}},        // empty target
		{{Target: "A", Surprised: nil}},                 // no surprise hosts
		{{Target: "A", Surprised: []string{"", "A"}}},   // empty + self only
	} {
		if st := l.DecayOnDisproof(paths, 0.5); st.Pairs != 0 || st.Pruned != 0 {
			t.Errorf("DecayOnDisproof(%+v) was not a no-op: %+v", paths, st)
		}
	}
	if after := l.CoOccurrences(); !reflect.DeepEqual(before, after) {
		t.Errorf("empty disproofs changed the counts:\n before=%+v\n after =%+v", before, after)
	}
}

// TestDecayOnDisproofConcurrent races DecayOnDisproof against the ingest Observe feed and CoOccurrences reads,
// as TestDecayConcurrent does for the time-based Decay. The learner is shared between the ingest feed and the
// hourly reconciliation that calls DecayOnDisproof, so the new method's map mutations must hold l.mu just like
// Decay's. Meaningful only under `go test -race`, where an unguarded map access is a hard failure.
func TestDecayOnDisproofConcurrent(t *testing.T) {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	l := NewCoOccurrenceLearner(10 * time.Minute)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 400; i++ {
			l.Observe(AlertObservation{Host: "h", At: at(base, i*2)})
			l.Observe(AlertObservation{Host: "g", At: at(base, i*2+1)}) // g follows h → pair (h,g) accrues
		}
		close(done)
	}()
	paths := []estate.DisproofPath{{Target: "h", Surprised: []string{"g"}}}
	for i := 0; i < 200; i++ {
		l.DecayOnDisproof(paths, 0.5)
		_ = l.CoOccurrences()
	}
	<-done
}
