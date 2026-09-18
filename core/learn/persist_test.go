package learn

import (
	"context"
	"reflect"
	"testing"

	"github.com/territory-grounder/grounder/core/estate"
)

// TestSnapshotRestoreRoundTrip is the fidelity proof for TG-388 face (c): a Snapshot must carry the learner's
// RAW decay-state floats (not the rounded CoOccurrences view), and Restore must reproduce the learner exactly
// — so a redeploy that loads the last snapshot rebuilds the identical learned tier instead of starting empty.
func TestSnapshotRestoreRoundTrip(t *testing.T) {
	l := NewCoOccurrenceLearner(0)
	observePairs(l, fixtureDay0, "A", "B", 8)
	observePairs(l, fixtureDay0.AddDate(0, 0, 100), "A", "E", 5)
	observePairs(l, fixtureDay0.AddDate(0, 0, 200), "C", "D", 3)
	// Decay by a factor that yields FRACTIONAL counts, so the test exercises the raw-float path the lossy
	// CoOccurrences() view (which rounds) would silently drop.
	l.DecayOnDisproof([]estate.DisproofPath{{Target: "A", Surprised: []string{"B"}}}, 0.3) // (A,B): 8 → 2.4

	snap := l.Snapshot()
	fractional := false
	for _, p := range snap.Pairs {
		if p.Count != float64(int64(p.Count)) {
			fractional = true
		}
	}
	if !fractional {
		t.Fatalf("no fractional count in the snapshot — the fixture failed to exercise the raw-float path")
	}

	restored := NewCoOccurrenceLearner(0)
	restored.Restore(snap)

	// Raw identity: the restored learner's own snapshot is byte-identical.
	if got := restored.Snapshot(); !reflect.DeepEqual(got, snap) {
		t.Errorf("Restore(Snapshot()) is not identity:\n want=%+v\n got =%+v", snap, got)
	}
	// Derived identity: the estate refresh would rebuild the exact same learned edges from the restored tier.
	origEdges, _ := l.LearnedSource().Edges(context.Background())
	restEdges, _ := restored.LearnedSource().Edges(context.Background())
	if !reflect.DeepEqual(origEdges, restEdges) {
		t.Errorf("restored learned edges differ from the original:\n orig=%+v\n rest=%+v", origEdges, restEdges)
	}
}

// TestRestoreEmptyAndCorruptDefensive — an empty snapshot yields an empty learner (first boot / empty DB), and
// a corrupt row (blank/self endpoint, non-positive count/trials) can never seed a phantom dependency.
func TestRestoreEmptyAndCorruptDefensive(t *testing.T) {
	l := NewCoOccurrenceLearner(0)
	observePairs(l, fixtureDay0, "A", "B", 4)
	l.Restore(Snapshot{})
	if got := l.CoOccurrences(); len(got) != 0 {
		t.Errorf("Restore(empty) left a non-empty learner: %+v", got)
	}

	l.Restore(Snapshot{
		Pairs: []PairObservation{
			{Primary: "X", Dependent: "Y", Count: 5, DelaySum: 10}, // the only valid row
			{Primary: "", Dependent: "Y", Count: 3},                // blank primary
			{Primary: "Z", Dependent: "Z", Count: 3},               // self-loop
			{Primary: "P", Dependent: "Q", Count: 0},               // non-positive count
		},
		Trials: []HostTrials{{Host: "X", Trials: 9}, {Host: "", Trials: 3}, {Host: "W", Trials: 0}},
	})
	co := l.CoOccurrences()
	if len(co) != 1 || co[0].Primary != "X" || co[0].Dependent != "Y" {
		t.Errorf("corrupt entries not filtered on restore: got %+v", co)
	}
}
