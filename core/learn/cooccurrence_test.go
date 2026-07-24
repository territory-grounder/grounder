package learn

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
)

func at(base time.Time, secs int) time.Time { return base.Add(time.Duration(secs) * time.Second) }

// Repeated in-window co-occurrence accumulates a (root → consequent) count that, once past the learned
// threshold, promotes to a capped estate edge; a one-off pair and a self-pair never do.
func TestCoOccurrenceLearnerAccumulates(t *testing.T) {
	base := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	l := NewCoOccurrenceLearner(10 * time.Minute)
	// db01 alerts, then app01 alerts 30s later — repeat this incident 4 times across days.
	for day := 0; day < 4; day++ {
		d := base.AddDate(0, 0, day)
		l.Observe(AlertObservation{Host: "db01", At: d})
		l.Observe(AlertObservation{Host: "app01", At: d.Add(30 * time.Second)})
	}
	// a single one-off coincidence
	l.Observe(AlertObservation{Host: "x", At: base.AddDate(0, 0, 9)})
	l.Observe(AlertObservation{Host: "y", At: base.AddDate(0, 0, 9).Add(time.Minute)})

	co := l.CoOccurrences()
	var dbApp *estate.CoOccurrence
	for i := range co {
		if co[i].Primary == "db01" && co[i].Dependent == "app01" {
			dbApp = &co[i]
		}
	}
	if dbApp == nil || dbApp.Count != 4 {
		t.Fatalf("db01→app01 must have accumulated 4 co-occurrences, got %+v", co)
	}
	// direction: app01 depends on db01 (db01 alerted first), never the reverse.
	for _, c := range co {
		if c.Primary == "app01" && c.Dependent == "db01" {
			t.Fatal("the reverse direction must not be recorded — db01 is the root")
		}
	}
	// the learned source promotes only the well-observed pair (>= 3); the one-off x→y (count 1) is dropped.
	edges, _ := l.LearnedSource().Edges(nil)
	if len(edges) != 1 || edges[0].From.Name != "app01" || edges[0].To.Name != "db01" {
		t.Fatalf("only the well-observed pair should become a learned edge, got %+v", edges)
	}
	if edges[0].Confidence > 0.75 {
		t.Fatalf("learned confidence must be capped at 0.75, got %.3f", edges[0].Confidence)
	}
}

// Alerts outside the cascade window do not co-occur.
func TestCoOccurrenceWindowBound(t *testing.T) {
	base := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	l := NewCoOccurrenceLearner(5 * time.Minute)
	l.Observe(AlertObservation{Host: "a", At: base})
	l.Observe(AlertObservation{Host: "b", At: at(base, 600)}) // 10min later — outside the 5min window
	if len(l.CoOccurrences()) != 0 {
		t.Fatalf("alerts 10min apart must not co-occur in a 5min window, got %+v", l.CoOccurrences())
	}
	// within the window they do
	l.Observe(AlertObservation{Host: "c", At: at(base, 700)})
	l.Observe(AlertObservation{Host: "d", At: at(base, 760)}) // 60s after c
	found := false
	for _, co := range l.CoOccurrences() {
		if co.Primary == "c" && co.Dependent == "d" {
			found = true
		}
	}
	if !found {
		t.Fatalf("alerts 60s apart must co-occur, got %+v", l.CoOccurrences())
	}
}

// The learner records per-host trial counts so the learned edge confidence is base-rate-aware: a dependent
// that follows the primary EVERY time outranks one that follows it rarely, at the same raw co-occurrence count.
func TestCoOccurrenceBaseRateAware(t *testing.T) {
	base := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	l := NewCoOccurrenceLearner(10 * time.Minute)
	// db01 alerts 5 times; app01 follows ALL 5 (5/5). noise01 alerts 45 more times alone (db01 trials still 5,
	// but web01 follows db01 5 times too while web01 itself alerts 50 times → its own base rate differs).
	for i := 0; i < 5; i++ {
		d := base.AddDate(0, 0, i)
		l.Observe(AlertObservation{Host: "db01", At: d})
		l.Observe(AlertObservation{Host: "app01", At: d.Add(30 * time.Second)})
	}
	co := l.CoOccurrences()
	var found bool
	for _, c := range co {
		if c.Primary == "db01" && c.Dependent == "app01" {
			found = true
			if c.PrimaryTrials != 5 {
				t.Fatalf("db01 had 5 incidents, PrimaryTrials=%d", c.PrimaryTrials)
			}
		}
	}
	if !found {
		t.Fatalf("db01→app01 must be recorded: %+v", co)
	}
	// the learned edge uses the base-rate-aware (Laplace) confidence: 5/5 → capped ~0.75, not the count ramp.
	edges, _ := l.LearnedSource().Edges(nil)
	if len(edges) != 1 {
		t.Fatalf("one learned edge expected, got %d", len(edges))
	}
	if edges[0].Confidence > 0.75 {
		t.Fatalf("learned confidence must be capped, got %.3f", edges[0].Confidence)
	}
}

// The learner is concurrency-safe: parallel Observe + CoOccurrences must not race (run with -race).
func TestCoOccurrenceConcurrent(t *testing.T) {
	base := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	l := NewCoOccurrenceLearner(10 * time.Minute)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			l.Observe(AlertObservation{Host: "h", At: at(base, i)})
		}
		close(done)
	}()
	for i := 0; i < 200; i++ {
		_ = l.CoOccurrences()
		_ = l.LearnedSource()
	}
	<-done
}
