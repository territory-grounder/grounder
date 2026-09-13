package learn

import (
	"testing"
	"time"
)

// dbAppCount returns the current (rounded) db01→app01 co-occurrence count from the learner's snapshot.
func dbAppCount(l *CoOccurrenceLearner) int {
	for _, c := range l.CoOccurrences() {
		if c.Primary == "db01" && c.Dependent == "app01" {
			return c.Count
		}
	}
	return 0
}

// A co-occurrence count HALVES over one half-life, and a pair that stops recurring keeps fading until it drops
// below the learned-edge threshold and finally ages out entirely — while the base-rate ratio is preserved.
func TestDecayHalvesCountsOverHalfLife(t *testing.T) {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	hl := 30 * 24 * time.Hour
	l := NewCoOccurrenceLearner(10 * time.Minute)
	// db01 → app01 observed 8 times on distinct days (count 8, well above the learned threshold of 3).
	for day := 0; day < 8; day++ {
		d := base.AddDate(0, 0, day)
		l.Observe(AlertObservation{Host: "db01", At: d})
		l.Observe(AlertObservation{Host: "app01", At: d.Add(30 * time.Second)})
	}
	if got := dbAppCount(l); got != 8 {
		t.Fatalf("db01→app01 must start at 8, got %d", got)
	}

	now := base.AddDate(0, 0, 8)
	l.Decay(now, hl) // first call establishes the baseline checkpoint (no decay yet)
	if got := dbAppCount(l); got != 8 {
		t.Fatalf("the baseline Decay call must not change counts, got %d", got)
	}
	// one half-life later, with NO fresh evidence, the count halves 8 → 4.
	l.Decay(now.Add(hl), hl)
	if got := dbAppCount(l); got != 4 {
		t.Fatalf("after one half-life the count must halve to 4, got %d", got)
	}
	// the learned edge still survives at count 4 (>= 3).
	if edges, _ := l.LearnedSource().Edges(nil); len(edges) != 1 {
		t.Fatalf("count 4 must still promote to one learned edge, got %d", len(edges))
	}
	// keep decaying with no reinforcement: 4 → 2 → below the threshold → aged out entirely.
	l.Decay(now.Add(2*hl), hl) // → 2
	if edges, _ := l.LearnedSource().Edges(nil); len(edges) != 0 {
		t.Fatalf("a decayed-below-threshold pair must promote no edge, got %d", len(edges))
	}
	l.Decay(now.Add(4*hl), hl) // → 0.5 → dropped
	l.Decay(now.Add(6*hl), hl) // → below the floor → pruned
	if got := dbAppCount(l); got != 0 {
		t.Fatalf("a long-unreinforced pair must age out of the tier entirely, got %d", got)
	}
}

// A non-positive half-life, and a clock that does not advance, are no-ops (the counts are untouched).
func TestDecayNoOpGuards(t *testing.T) {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	l := NewCoOccurrenceLearner(10 * time.Minute)
	for day := 0; day < 5; day++ {
		d := base.AddDate(0, 0, day)
		l.Observe(AlertObservation{Host: "db01", At: d})
		l.Observe(AlertObservation{Host: "app01", At: d.Add(30 * time.Second)})
	}
	if st := l.Decay(base, 0); st.Pairs != 0 || st.Pruned != 0 {
		t.Fatalf("a non-positive half-life must be a no-op, got %+v", st)
	}
	l.Decay(base, 24*time.Hour)              // baseline
	if st := l.Decay(base, 24*time.Hour); st.Pairs != 0 || st.Pruned != 0 { // no elapsed time
		t.Fatalf("a non-advancing clock must be a no-op, got %+v", st)
	}
	if got := dbAppCount(l); got != 5 {
		t.Fatalf("no-op decays must leave the count at 5, got %d", got)
	}
}

// Decay is concurrency-safe alongside Observe + CoOccurrences (run with -race).
func TestDecayConcurrent(t *testing.T) {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	l := NewCoOccurrenceLearner(10 * time.Minute)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 400; i++ {
			l.Observe(AlertObservation{Host: "h", At: at(base, i)})
		}
		close(done)
	}()
	for i := 0; i < 200; i++ {
		l.Decay(at(base, i*10), time.Hour)
		_ = l.CoOccurrences()
	}
	<-done
}
