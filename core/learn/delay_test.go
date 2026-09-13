package learn

import (
	"testing"
	"time"
)

// TG-188 — the co-occurrence learner already had both timestamps at the moment it counted a pair, and
// discarded the gap. That gap IS the propagation delay. These pin the estimate and the one property that
// separates a real average from a stamped constant: it survives a decay half-life unchanged, because a
// mean of gaps does not age even as the evidence behind it fades.
//
// The fixtures use a SHORT window and incidents spaced far apart, deliberately: the learner pairs a
// consequent with EVERY earlier different-host alert still in the window, so a wide window would pair
// A's incident-1 alert with B's incident-2 alert and the expected mean would no longer be the obvious
// one. (My first draft of this test made exactly that error; the learner was right and the fixture was
// wrong.) Short window + far-apart incidents keeps each pairing unambiguous.

func dly(base time.Time, secs int) time.Time { return base.Add(time.Duration(secs) * time.Second) }

func TestMeanDelayIsThePerPairAverageGap(t *testing.T) {
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	l := NewCoOccurrenceLearner(60 * time.Second)

	// incident 1: A→B gap 10, A→C gap 5
	l.Observe(AlertObservation{Host: "A", At: dly(base, 0)})
	l.Observe(AlertObservation{Host: "C", At: dly(base, 5)})
	l.Observe(AlertObservation{Host: "B", At: dly(base, 10)})
	// incident 2, well outside the 60s window: A→B gap 30
	l.Observe(AlertObservation{Host: "A", At: dly(base, 1000)})
	l.Observe(AlertObservation{Host: "B", At: dly(base, 1030)})

	got := map[[2]string]float64{}
	for _, co := range l.CoOccurrences() {
		got[[2]string{co.Primary, co.Dependent}] = co.MeanDelaySeconds
	}
	if d := got[[2]string{"A", "B"}]; d != 20 {
		t.Errorf("A→B mean delay = %v, want 20 (gaps 10 and 30) — a count-only stamp would report 0 here", d)
	}
	if d := got[[2]string{"A", "C"}]; d != 5 {
		t.Errorf("A→C mean delay = %v, want 5", d)
	}
}

// THE LOAD-BEARING PROPERTY: decay preserves the mean. delaySum and counts scale by the same factor, so a
// half-life that halves the evidentiary weight must not move the average gap by a single second.
func TestMeanDelayIsUnchangedByADecayHalfLife(t *testing.T) {
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	l := NewCoOccurrenceLearner(60 * time.Second)
	// three isolated A→B incidents, gaps 12/12/24 ⇒ mean 16s
	for i, gap := range []int{12, 12, 24} {
		root := dly(base, i*1000)
		l.Observe(AlertObservation{Host: "A", At: root})
		l.Observe(AlertObservation{Host: "B", At: root.Add(time.Duration(gap) * time.Second)})
	}
	before := l.CoOccurrences()[0].MeanDelaySeconds
	if before != 16 {
		t.Fatalf("pre-decay mean = %v, want 16", before)
	}
	l.Decay(dly(base, 5000), time.Hour)                // baseline
	l.Decay(dly(base, 5000).Add(time.Hour), time.Hour) // one half-life

	after := l.CoOccurrences()
	if len(after) == 0 {
		t.Fatal("the pair was pruned by a single half-life — it should have halved in weight, not vanished")
	}
	if after[0].MeanDelaySeconds != 16 {
		t.Errorf("post-half-life mean = %v, want 16 unchanged — the delay accumulator is not decaying in "+
			"lockstep with the count, so the average drifts as evidence ages", after[0].MeanDelaySeconds)
	}
}

// A pruned pair takes its delay with it. This asserts the INTERNAL invariant, not the output, on purpose:
// CoOccurrences iterates `counts`, so an orphaned delaySum entry is INVISIBLE through the public surface —
// the first version of this test iterated CoOccurrences and passed even when the prune was deleted (the
// mutation survived). The real cost of leaking it is unbounded memory as pairs churn over a production
// lifetime, and the only place that shows is delaySum growing past counts. So: after any decay, the two
// maps must have exactly the same keys.
func TestDelaySumStaysInLockstepWithCountsAcrossDecay(t *testing.T) {
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	l := NewCoOccurrenceLearner(60 * time.Second)
	// Two pairs; one will be pruned, one kept, so a leak makes the counts differ.
	l.Observe(AlertObservation{Host: "A", At: dly(base, 0)})
	l.Observe(AlertObservation{Host: "B", At: dly(base, 8)})
	l.Observe(AlertObservation{Host: "X", At: dly(base, 2000)})
	l.Observe(AlertObservation{Host: "Y", At: dly(base, 2005)})

	l.Decay(dly(base, 3000), time.Hour) // baseline
	// Age enough to prune the older pair but (with a long half-life relative to the count) not necessarily
	// both — the point is only that whatever survives is IDENTICAL between the two maps.
	l.Decay(dly(base, 3000).Add(48*time.Hour), time.Hour)

	l.mu.Lock()
	nc, nd := len(l.counts), len(l.delaySum)
	// every surviving delaySum key must have a counts key and vice versa
	sameKeys := nc == nd
	for k := range l.delaySum {
		if _, ok := l.counts[k]; !ok {
			sameKeys = false
		}
	}
	l.mu.Unlock()
	if !sameKeys {
		t.Errorf("counts has %d pair(s) but delaySum has %d — a pruned pair left its propagation estimate "+
			"behind, which grows memory unbounded over a production lifetime and is invisible through "+
			"CoOccurrences (why this asserts the internal maps, not the output)", nc, nd)
	}
}
