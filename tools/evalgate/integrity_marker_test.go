package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/eval/gate"
)

// TG-678 through the REAL binary (runCLI re-execs main): the counters-recorded discriminator and the legacy
// disclosure as the shell sees them on --verify-integrity and on the change gate's arm checks.

func markerDims() map[string]float64 {
	return map[string]float64{"appropriate_band": 4, "correct_diagnosis": 4, "evidence_grounded": 4, "sensible_proposal": 4, "falsifiable_prediction": 4}
}

// A fresh (stamped) card whose arm errored: DEGRADED, exit 1, the cause named.
func TestCLI_FreshErroredCardIsDegraded(t *testing.T) {
	dir := t.TempDir()
	card := writeJSON(t, dir, "card.json", gate.Scorecard{N: 20, Judged: 18, Errors: 2, FirstErr: "connect: connection refused", CountersRecorded: true, Overall: 3.5, DimMeans: markerDims()})
	code, out := runCLI(t, "--verify-integrity", card)
	if code != 1 || !strings.Contains(out, "INTEGRITY: DEGRADED") || !strings.Contains(out, "2/20 session(s) errored") || !strings.Contains(out, "connection refused") {
		t.Fatalf("want exit 1 DEGRADED naming the errors, got %d:\n%s", code, out)
	}
}

// A fresh (stamped) card that judged NOTHING beside a positive overall: DEGRADED, exit 1 — it is not legacy.
// Before this change the same file printed "INTEGRITY: OK" (the open-ended Judged==0 skip).
func TestCLI_FreshZeroJudgedCardIsDegradedNotLegacy(t *testing.T) {
	dir := t.TempDir()
	card := writeJSON(t, dir, "card.json", gate.Scorecard{N: 20, Judged: 0, Errors: 0, CountersRecorded: true, Overall: 3.5, DimMeans: markerDims()})
	code, out := runCLI(t, "--verify-integrity", card, "--expect-n", "20")
	if code != 1 || !strings.Contains(out, "only 0/20 session(s) judged") {
		t.Fatalf("a stamped zero-judged card must be refused; got exit %d:\n%s", code, out)
	}
	if strings.Contains(out, "LEGACY") {
		t.Errorf("a fresh card must not be called legacy:\n%s", out)
	}
	// And as a --candidate it is an integrity error (exit 2), never pooled.
	code, out = runCLI(t, "--mode", "change", "--base", card, "--candidate", card, "--controls", cleanControls(t, dir))
	if code != 2 || !strings.Contains(out, "candidate arm integrity failed") {
		t.Fatalf("want exit 2 on the candidate arm, got %d:\n%s", code, out)
	}
}

// A legacy card (no stamp, judged 0, overall > 0): tolerated — INTEGRITY: OK, exit 0 — and DISCLOSED on stdout.
func TestCLI_LegacyCardIsToleratedAndDisclosed(t *testing.T) {
	dir := t.TempDir()
	card := writeJSON(t, dir, "card.json", gate.Scorecard{N: 20, Judged: 0, Errors: 0, Overall: 3.5, DimMeans: markerDims()})
	code, out := runCLI(t, "--verify-integrity", card, "--expect-n", "20")
	if code != 0 || !strings.Contains(out, "INTEGRITY: OK") {
		t.Fatalf("a legacy card must still verify, got exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "INTEGRITY: note — scorecard run 1: LEGACY card") {
		t.Fatalf("the tolerance must be disclosed on stdout:\n%s", out)
	}
	// The archived 2026-07-30 record (a real legacy pooled card) still drives the change gate and is disclosed.
	archivedCard := filepath.Join(archivedChangeRun, "scorecard.json")
	code, out = runCLI(t, "--mode", "change", "--base", archivedCard, "--candidate", archivedCard, "--controls", cleanControls(t, dir))
	if code == 2 {
		t.Fatalf("the archived legacy record must not become an integrity error:\n%s", out)
	}
	if strings.Count(out, "LEGACY card") < 2 {
		t.Errorf("both arms are legacy cards and both must be disclosed:\n%s", out)
	}
}

// The archive writer/reader round trip on the CLI's own path: writeArchive persists a pooled card whose sums,
// stamp and provenance LoadScorecard reads back.
func TestArchiveRoundTripKeepsThePooledCounters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026-11-07-change-deadbeef0000")
	fresh := func(judged, errs int) gate.Scorecard {
		return gate.Scorecard{N: 20, Judged: judged, Errors: errs, CountersRecorded: true, Overall: 4.0, RubricVersion: "2026-10-23.1", DimMeans: markerDims()}
	}
	pooled := gate.Pool([]gate.Scorecard{fresh(20, 0), fresh(20, 0), fresh(20, 0)})
	comparator := gate.PoolToBaseline([]gate.Scorecard{fresh(20, 0)}, "2026-11-07", "deadbeef")
	if err := writeArchive(dir, comparator, pooled, gate.Verdict{Outcome: gate.OutcomePass, Pass: true}); err != nil {
		t.Fatal(err)
	}
	back, err := gate.LoadScorecard(filepath.Join(dir, "scorecard.json"))
	if err != nil {
		t.Fatal(err)
	}
	if back.Judged != 60 || back.Errors != 0 || back.Runs != 3 || !back.CountersRecorded || len(back.RunProvenance) != 3 || back.RunProvenance[2].RubricVersion != "2026-10-23.1" {
		t.Fatalf("the archived pooled card lost its counters: %+v", back)
	}
	if probs, notes := gate.VerifyIntegrity("archived", []gate.Scorecard{back}, 0), gate.IntegrityDisclosures("archived", []gate.Scorecard{back}); len(probs) != 0 || len(notes) != 0 {
		t.Errorf("a fresh archived pooled card verifies silently: %v %v", probs, notes)
	}
	cmp, err := gate.LoadBaseline(filepath.Join(dir, "comparator.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Scorecard.Judged != 20 || !cmp.Scorecard.CountersRecorded || cmp.Scorecard.Runs != 1 {
		t.Errorf("the archived comparator lost its counters: %+v", cmp.Scorecard)
	}
}
