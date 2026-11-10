package axis

import (
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
)

// TG-633: A6 reads per RUN when triage_run covers the window, and says "first runs only" when it does not.
func TestA6SourcePrefersRunLevelWhenCovered(t *testing.T) {
	since := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	a := db.AxisAgg{Since: since, MeanSteps: 5, StepsN: 40, DecisionN: 40, DecisionMedianMs: 9000,
		RunMeanSteps: 4, RunStepsN: 130, RunDecisionN: 128, RunDecisionMedianMs: 8000, RunDecisionP95Ms: 30000,
		RunsSince: since.Add(-72 * time.Hour)}
	got := a6Source(a, Score(a, 14*24*time.Hour))
	if got.steps != 4 || got.stepsN != 130 || got.decisionN != 128 || got.medianMs != 8000 || got.note != "" ||
		!strings.Contains(got.stepsUnit, "runs") {
		t.Fatalf("a covered window must read the run-level numbers, got %+v", got)
	}

	a.RunsSince = since.Add(time.Hour) // the table started after the window opened
	got = a6Source(a, Score(a, 14*24*time.Hour))
	if got.steps != 5 || got.stepsN != 40 || !strings.Contains(got.note, "first runs only") || !strings.Contains(got.note, "2026-11-10T01:00Z") {
		t.Fatalf("a partly covered window must fall back to session numbers and say why, got %+v", got)
	}

	a.RunsSince = time.Time{}
	if got = a6Source(a, Score(a, 14*24*time.Hour)); got.stepsN != 40 || !strings.Contains(got.note, "do not cover") {
		t.Fatalf("no run records: session numbers with the note, got %+v", got)
	}
}
