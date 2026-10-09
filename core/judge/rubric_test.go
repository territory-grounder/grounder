package judge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenSession is the canonical record whose rendered prompt is pinned in testdata/golden_prompt.txt.
// The golden was captured from the pre-refactor Prompt() (the fixed rubric text inline in judge.go); this
// exact session is re-used so the byte-identity assertion is meaningful.
var goldenSession = Session{
	Ref: "TG-GOLDEN", AlertRule: "HostDown", Host: "web01", Severity: "critical",
	Band: "POLL_PAUSE", Proposed: true, Op: "restart-service", ActionID: "act-123",
	Prediction: "alert clears within 10m", Predicted: true,
	Evidence: []string{"tool-1", "tool-2"}, Conclusion: "device is administratively DISABLED",
	Decisions: []string{"gate:allow"}, Outcome: "proposed", Mutated: false,
}

// TestPromptMatchesGolden is the NO-SCORING-CHANGE proof: after moving the rubric text out of judge.go and
// into the embedded rubric.json (the one source), Prompt() still renders byte-for-byte what it rendered
// before. If a single character of the rubric drifts, this fails — the eval gate's judge is unchanged.
func TestPromptMatchesGolden(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "golden_prompt.txt"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	got := Prompt(goldenSession)
	if got != string(want) {
		t.Fatalf("Prompt() drifted from the golden (rubric wording changed).\n--- got (%d bytes) ---\n%s\n--- want (%d bytes) ---\n%s", len(got), got, len(want), string(want))
	}
}

// TestRubricIsSingleSource proves the Go side reads ONE rubric: the embedded bytes equal the on-disk
// rubric.json, Prompt() is built from that rubric's text, and Dimensions is sourced from it (not a
// re-declared literal). Combined with the Python-side path assertion below (and test_rubric.py), this
// pins that the Go embed and the Python read resolve to the same bytes.
func TestRubricIsSingleSource(t *testing.T) {
	onDisk, err := os.ReadFile("rubric.json")
	if err != nil {
		t.Fatalf("read rubric.json: %v", err)
	}
	if string(RubricJSON()) != string(onDisk) {
		t.Fatal("embedded rubric.json != on-disk rubric.json (the embed drifted from the source file)")
	}
	p := Prompt(goldenSession)
	if !strings.Contains(p, rubric.Guidance) {
		t.Fatal("Prompt() must render the rubric's Guidance verbatim (it is not built from the one source)")
	}
	if !strings.Contains(p, rubric.HollowProposalRule) {
		t.Fatal("Prompt() must render the rubric's HollowProposalRule verbatim (it is not built from the one source)")
	}
	want := []string{"correct_diagnosis", "evidence_grounded", "sensible_proposal", "appropriate_band", "falsifiable_prediction"}
	if len(Dimensions) != len(want) {
		t.Fatalf("Dimensions must be the 5 canonical axes from the rubric; got %v", Dimensions)
	}
	for i, d := range want {
		if Dimensions[i] != d {
			t.Fatalf("Dimensions[%d]=%q want %q (order must match the rubric)", i, Dimensions[i], d)
		}
	}
}

// TestDefaultParams pins the ONE JudgeParams every caller constructs from: the "primary" reasoning tier at
// temperature 0 (deterministic judging), sourced from the same rubric.json Python reads.
func TestDefaultParams(t *testing.T) {
	p := DefaultParams()
	if p.Model != "primary" {
		t.Fatalf("judge model must be the canonical primary tier; got %q", p.Model)
	}
	if p.Temperature != 0 {
		t.Fatalf("judge temperature must be 0 (deterministic); got %v", p.Temperature)
	}
	if p.Seed != 0 {
		t.Fatalf("default seed must be 0; got %d", p.Seed)
	}
	// DefaultParams must be the embedded rubric's params, not a private copy.
	if DefaultParams() != rubric.Params {
		t.Fatal("DefaultParams must be sourced from the embedded rubric.json (the one source)")
	}
}

// TestShadowbenchReadsCanonicalRubric proves the Python shadowbench judge reads the SAME rubric.json this
// package embeds — it points at core/judge/rubric.json and no longer carries a hand-copied guidance/rule.
// The test runs from the core/judge package dir, so ../../tools/shadowbench is the repo's shadowbench dir.
func TestShadowbenchReadsCanonicalRubric(t *testing.T) {
	for _, rel := range []string{
		filepath.Join("..", "..", "tools", "shadowbench", "judge.py"),
		filepath.Join("..", "..", "tools", "shadowbench", "_driver.py"),
	} {
		src, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		s := string(src)
		// Points at the canonical file (path components, robust to os.path.join style).
		if !strings.Contains(s, "rubric.json") || !strings.Contains(s, "judge") {
			t.Fatalf("%s must reference the canonical core/judge/rubric.json path", rel)
		}
		// The old hand-copied guidance sentence must be GONE — the calibration text now comes from the file.
		if strings.Contains(s, "an irreversible/critical action must NOT be silently AUTO") {
			t.Fatalf("%s still hard-codes a copy of the rubric guidance (must read it from rubric.json)", rel)
		}
	}
	// _driver.py must not re-declare the dimension list either — it imports it from the judge module.
	drv, err := os.ReadFile(filepath.Join("..", "..", "tools", "shadowbench", "_driver.py"))
	if err != nil {
		t.Fatalf("read _driver.py: %v", err)
	}
	if strings.Contains(string(drv), "DIMENSIONS = [") {
		t.Fatal("_driver.py must not re-declare DIMENSIONS — import it from the judge module (the one source)")
	}
}
