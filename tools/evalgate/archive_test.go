package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/territory-grounder/grounder/eval/gate"
)

func TestWriteArchive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026-07-30-change-abc123")
	v := gate.Verdict{Pass: false, Reasons: []string{"proposal_rate 0.00 < absolute floor 0.25"}}
	if err := writeArchive(dir, gate.Baseline{MeasuredAt: "2026-07-30"}, gate.Scorecard{N: 20}, v); err != nil {
		t.Fatalf("writeArchive: %v", err)
	}
	for _, f := range []string{"comparator.json", "scorecard.json", "verdict.json"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil || len(b) == 0 {
			t.Fatalf("%s: %v (len %d)", f, err, len(b))
		}
	}
}
