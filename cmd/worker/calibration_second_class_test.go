package main

import (
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/metrics"
)

// THE COMPOSITION ROOT (TG-335). temporal/calibrate proves EmitFor carries whatever class it is given;
// only this proves main() actually runs a second job with the second class. Without it, OutcomeDiagnosisCorrect
// stays what it was for months — a declared constant that ClampCalibrationOutcome accepts and nothing emits.
//
// Comment lines are stripped, and TestTheSecondClassGuardIgnoresProse holds that honest: an earlier guard in
// this repo passed by matching its own explanatory comment.
func TestMainPublishesBothCalibrationOutcomes(t *testing.T) {
	src := stripCalibComments(readWorkerMain(t))

	if !strings.Contains(src, "calibratejob.EmitFor(metrics.OutcomeDiagnosisCorrect") {
		t.Error("main.go never emits the diagnosis_correct reference class. The confidence alerts tell an " +
			"operator to compare blast-radius exactness against diagnosis correctness before calling the " +
			"agent overconfident, and with one class published that comparison is a dead end.")
	}
	if !strings.Contains(src, "calibratejob.EmitTo(obsRegistry)") {
		t.Error("main.go no longer emits the blast_radius_exact class — the original curve every existing " +
			"alert and dashboard reads")
	}
	// The two jobs must read DIFFERENT populations. Emitting the same samples under two labels would be
	// worse than one label: it manufactures an apparent comparison between identical numbers.
	if !strings.Contains(src, "db.DiagnosisSampleReader{") {
		t.Error("the diagnosis curve does not use DiagnosisSampleReader, so both classes would score the " +
			"same blast-radius population under two different names")
	}
}

func TestTheSecondClassGuardIgnoresProse(t *testing.T) {
	prose := "// calibratejob.EmitFor(metrics.OutcomeDiagnosisCorrect, obsRegistry)\nfunc main() {}\n"
	if got := stripCalibComments(prose); strings.Contains(got, "calibratejob.EmitFor(metrics.OutcomeDiagnosisCorrect") {
		t.Fatalf("stripCalibComments left commented-out code in place; got %q", got)
	}
}

// Both classes this deployment publishes must be in the closed set, or the label stops being a reference
// class. Enumerates rather than spot-checking the one just added.
func TestEveryPublishedOutcomeIsInTheClosedSet(t *testing.T) {
	for _, c := range []string{metrics.OutcomeBlastRadiusExact, metrics.OutcomeDiagnosisCorrect} {
		if got := metrics.ClampCalibrationOutcome(c); got != c {
			t.Errorf("%q is published but clamps to %q — it would appear on /metrics as something else", c, got)
		}
	}
}

func readWorkerMain(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("main.go is empty — the assertions above would be vacuous")
	}
	return string(b)
}

func stripCalibComments(src string) string {
	var b strings.Builder
	for _, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}
