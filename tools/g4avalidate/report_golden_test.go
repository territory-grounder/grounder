package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/axis"
	"github.com/territory-grounder/grounder/core/db"
)

// THE PRODUCER CONTRACT (TG-672): the validator's Report is a subset of core/axis.BenchmarkReport's JSON. A field
// renamed on either side would make the validator read "absent" and FAIL CLOSED — safe, but silent until Phase 3.
// This test marshals a real g4a report and reads it back through LoadReport.
func TestValidatorReadsARealAxisscoreG4AReport(t *testing.T) {
	since := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 11, 20, 17, 0, 0, 0, time.UTC)
	census := db.WindowCensus{Since: since, Until: until,
		RubricVersions: map[string]int{"2026-11-08.1": 900}, HarnessFPs: map[string]int{"b": 140}, InstrumentIDs: map[string]int{"a": 140},
		TriagesBySite: map[string]int{}, FaultsBySite: map[string]int{}}
	rep := axis.BenchmarkBar(axis.BarG4A, census, "2026-11-08.1", map[string]db.AxisAgg{}, db.AxisAgg{})
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := LoadReport(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Bar != "g4a" || r.Verdict != rep.Verdict || !r.Until.Equal(until) || len(r.Sites) != len(rep.Sites) ||
		len(r.RubricVersions) != 1 || r.RubricVersions[0].Value != "2026-11-08.1" || len(r.HarnessFPs) != 1 || len(r.InstrumentIDs) != 1 ||
		r.LedgerGaps == nil || *r.LedgerGaps != 0 || len(r.Failures) != len(rep.Failures) {
		t.Fatalf("the validator read a different report than axisscore wrote:\nread  %+v\nwrote bar=%s verdict=%s sites=%v failures=%d", r, rep.Bar, rep.Verdict, rep.Sites, len(rep.Failures))
	}
}
