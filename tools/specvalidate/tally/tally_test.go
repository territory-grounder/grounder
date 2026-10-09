package tally

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture is one synthetic lattice, stated field by field so each test says exactly the condition it is
// about and nothing else. The zero value plus build's defaults is the BENIGN case: two specs, valid closed
// vocabularies everywhere, an index with a table row per spec and the marker pair present.
type fixture struct {
	noMarkers bool   // omit the BEGIN/END marker pair from the index
	noTasks   bool   // write NO tasks.json anywhere — the blind-lattice case
	tasksJSON string // override spec/031-alpha/tasks.json verbatim
	blockEdit func(block string) string
}

// build writes the tree and returns its root. Every file the tally reads is written explicitly, so a test
// that breaks one input is testing that input and nothing else.
func build(t *testing.T, f fixture) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if !f.noTasks {
		alpha := f.tasksJSON
		if alpha == "" {
			alpha = `{"spec":"031-alpha","tasks":[
				{"id":"T-031-1","status":"completed"},
				{"id":"T-031-2","status":"pending"},
				{"id":"T-031-3","status":"blocked"}]}`
		}
		write("spec/031-alpha/tasks.json", alpha)
		write("spec/032-beta/tasks.json", `{"spec":"032-beta","tasks":[
			{"id":"T-032-1","status":"completed"},
			{"id":"T-032-2","status":"pending"}]}`)
	}
	write("spec/031-alpha/acceptance/_test_mapping.json", `{"feature":"a.feature","scenarios":[
		{"name":"s1","req":"REQ-001","status":"present","test":"TestS1"},
		{"name":"s2","req":"REQ-002","status":"pending","test":""}]}`)
	write("spec/032-beta/acceptance/_test_mapping.json", `{"feature":"b.feature","scenarios":[
		{"name":"s3","req":"REQ-101","status":"retrospective_gap","test":""}]}`)

	index := "| Spec | Title | Status |\n|---|---|---|\n" +
		"| [spec/031](031-alpha/) | alpha | **Ratified** |\n" +
		"| [spec/032](032-beta/) | beta | **Draft** |\n\nprose above the block\n\n"
	if !f.noMarkers {
		index += BeginMarker + "\n" + EndMarker + "\n"
	}
	index += "\nprose below the block\n"
	write("spec/00-INDEX.md", index)

	if f.blockEdit != nil {
		p := filepath.Join(root, "spec", "00-INDEX.md")
		if err := Run(root, "write"); err != nil {
			t.Fatalf("write before blockEdit: %v", err)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.blockEdit(string(b))), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// (i) The round trip: a written block passes check, byte for byte, run after run — the determinism the
// whole gate rests on (a timestamp or map-order wobble here would red every pipeline).
func TestWriteThenCheckPasses(t *testing.T) {
	root := build(t, fixture{})
	if err := Run(root, "write"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := Run(root, "check"); err != nil {
		t.Fatalf("check after write must pass: %v", err)
	}
	if err := Run(root, "check"); err != nil {
		t.Fatalf("second check must also pass (determinism): %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "spec", "00-INDEX.md"))
	for _, want := range []string{
		"2 completed / 2 pending / 1 blocked of 5 across 2 specs",
		"1 present / 1 pending / 1 retrospective_gap of 3",
		"1 Ratified / 0 Approved / 1 Draft of 2 rows",
		"031-alpha 1",
		"032-beta 1",
		"hand edits are rejected",
		"prose above the block", // surrounding prose survives the write
		"prose below the block",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("written index missing %q", want)
		}
	}
}

// (ii) The killing mutation for the gate itself: ONE hand-edited digit in the block must fail check, and
// the failure must show the differing line both ways.
func TestHandEditedNumberFailsCheck(t *testing.T) {
	root := build(t, fixture{blockEdit: func(s string) string {
		edited := strings.Replace(s, "2 completed", "3 completed", 1)
		if edited == s {
			t.Fatal("blockEdit found nothing to edit — the mutation would prove nothing")
		}
		return edited
	}})
	err := Run(root, "check")
	if err == nil {
		t.Fatal("a hand-edited number in the block must fail check")
	}
	msg := err.Error()
	if !strings.Contains(msg, "TALLY DRIFT") {
		t.Errorf("drift failure must say TALLY DRIFT, got: %v", msg)
	}
	if !strings.Contains(msg, "3 completed") || !strings.Contains(msg, "2 completed") {
		t.Errorf("drift failure must show both versions of the first differing line, got: %v", msg)
	}
}

// (iii) The blindness guard: a spec/ tree with ZERO tasks.json is a failed measurement, not a lattice with
// zero tasks — check must refuse with its own distinct message, not pass vacuously or report drift.
func TestZeroTasksJSONIsBlindNotZero(t *testing.T) {
	root := build(t, fixture{noTasks: true})
	for _, mode := range []string{"check", "write"} {
		err := Run(root, mode)
		if err == nil {
			t.Fatalf("%s over 0 tasks.json must fail", mode)
		}
		if !strings.Contains(err.Error(), "TALLY BLIND") {
			t.Errorf("%s: want the distinct TALLY BLIND message, got: %v", mode, err)
		}
		if strings.Contains(err.Error(), "TALLY DRIFT") {
			t.Errorf("%s: blindness must not be reported as drift: %v", mode, err)
		}
	}
}

// (iv) Markers absent: a distinct failure naming the file — nothing to byte-compare, so it must not read
// as drift, and it must not pass.
func TestMissingMarkersIsDistinctFailure(t *testing.T) {
	root := build(t, fixture{noMarkers: true})
	err := Run(root, "check")
	if err == nil {
		t.Fatal("check with no marker pair must fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "TALLY MARKERS MISSING") {
		t.Errorf("want the distinct markers-missing message, got: %v", msg)
	}
	if !strings.Contains(msg, "00-INDEX.md") {
		t.Errorf("markers-missing failure must name the file, got: %v", msg)
	}
	if strings.Contains(msg, "TALLY DRIFT") {
		t.Errorf("missing markers must not be reported as drift: %v", msg)
	}
}

// (v) The closed task-status vocabulary: an unknown status is a HARD ERROR naming file and task — silently
// binning it would put a wrong number in the index with this tool's authority behind it. ("done" is the
// historically-real offender: it and "completed" were once the same state under two words.)
func TestUnknownTaskStatusIsHardError(t *testing.T) {
	root := build(t, fixture{tasksJSON: `{"spec":"031-alpha","tasks":[
		{"id":"T-031-1","status":"completed"},
		{"id":"T-031-2","status":"done"}]}`})
	err := Run(root, "check")
	if err == nil {
		t.Fatal("an unknown task status must be a hard error")
	}
	msg := err.Error()
	if !strings.Contains(msg, `"done"`) {
		t.Errorf("error must name the offending status, got: %v", msg)
	}
	if !strings.Contains(msg, "T-031-2") {
		t.Errorf("error must name the offending task, got: %v", msg)
	}
	if !strings.Contains(msg, filepath.Join("031-alpha", "tasks.json")) {
		t.Errorf("error must name the offending file, got: %v", msg)
	}
}

// An unknown mode is rejected outright — the CLI's usage/exit-2 path depends on Run never guessing.
func TestUnknownModeRejected(t *testing.T) {
	root := build(t, fixture{})
	if err := Run(root, "frobnicate"); err == nil {
		t.Fatal("unknown mode must be an error")
	}
}

// An unreadable-but-PRESENT lattice file must fail the tally, never shrink it: silently skipping one
// tasks.json would under-report with the tool's authority — absence and refusal are different states.
// The file is replaced by a DIRECTORY of the same name so the read fails even when tests run as root
// (chmod 000 does not stop root; EISDIR stops everyone).
func TestUnreadableLatticeFileFailsNotShrinks(t *testing.T) {
	root := build(t, fixture{})
	tp := filepath.Join(root, "spec", "031-alpha", "tasks.json")
	if err := os.Remove(tp); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tp, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := Run(root, "check")
	if err == nil {
		t.Fatal("an unreadable tasks.json must fail the tally, not shrink it to the readable subset")
	}
	if !strings.Contains(err.Error(), "cannot read") || !strings.Contains(err.Error(), "tasks.json") {
		t.Errorf("error must name the unreadable file, got: %v", err)
	}
}
