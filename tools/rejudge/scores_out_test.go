package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/territory-grounder/grounder/eval"
)

// TG-584: the per-session dump is what a judge-tier A/B pairs on; an empty path writes nothing (the default
// invocation is byte-identical to before the flag existed), a path round-trips every score.
func TestWriteScoresRoundTripAndEmptyPathWritesNothing(t *testing.T) {
	dir := t.TempDir()
	if err := writeScores("", []eval.Score{{Ref: "x"}}); err != nil {
		t.Fatalf("empty path must be a no-op, got %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("empty path must write no file, found %d", len(entries))
	}
	in := []eval.Score{
		{Ref: "TG-1", Scores: map[string]int{"correct_diagnosis": 5, "appropriate_band": 3}, Comment: "a"},
		{Ref: "TG-2", Scores: map[string]int{"correct_diagnosis": 2}, Comment: "b"},
	}
	path := filepath.Join(dir, "scores.json")
	if err := writeScores(path, in); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []eval.Score
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Ref != "TG-1" || out[0].Scores["appropriate_band"] != 3 || out[1].Comment != "b" {
		t.Fatalf("round trip lost data: %+v", out)
	}
}
