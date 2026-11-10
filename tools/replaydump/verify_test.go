package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/territory-grounder/grounder/eval"
)

// -verify REFUSES an empty recording (exit 1) and passes a real one; the comparison is exact — a recording whose
// stored id does not match what the arm serves, or whose output differs, is a mismatch.
func TestVerifyRefusesEmptyAndIsExact(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`{"ExternalRef":"x","Calls":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := runVerify(empty, filepath.Join(dir, "no-corpus.json")); rc != 1 {
		t.Fatalf("an empty recording must be refused with exit 1, got %d", rc)
	}
	good := eval.RecordedSession{ExternalRef: "TG-v", Calls: []eval.RecordedCall{
		{Cycle: 1, Tool: "get-device-status", EvidenceID: "lnms-dev-web01", Output: "up", Success: true},
		{Cycle: 2, Tool: "get-active-alerts", EvidenceID: "lnms-alerts-", Output: "none", Success: true},
		{Cycle: 3, Tool: "check-host-disk", EvidenceID: "check-host-disk-web01", Output: "clipped", Success: false, Truncated: true},
	}}
	if bad := verifyRecording(good, eval.Incident{ExternalRef: "TG-v"}); bad != 0 {
		t.Fatalf("a faithful recording must verify clean, got %d mismatch(es)", bad)
	}
	unknownFamily := eval.RecordedSession{ExternalRef: "TG-v", Calls: []eval.RecordedCall{
		{Cycle: 1, Tool: "get-device-status", EvidenceID: "mystery-42", Output: "up", Success: true},
	}}
	if bad := verifyRecording(unknownFamily, eval.Incident{ExternalRef: "TG-v"}); bad != 1 {
		t.Fatalf("an id in no known family must be a mismatch, got %d", bad)
	}
	twoSameTarget := eval.RecordedSession{ExternalRef: "TG-v", Calls: []eval.RecordedCall{
		{Cycle: 1, Tool: "get-device-status", EvidenceID: "lnms-dev-web01", Output: "up", Success: true},
		{Cycle: 2, Tool: "get-device-status", EvidenceID: "lnms-dev-web01", Output: "down", Success: true},
	}}
	if bad := verifyRecording(twoSameTarget, eval.Incident{ExternalRef: "TG-v"}); bad != 0 {
		t.Fatalf("two observations of one target must both verify in order, got %d mismatch(es)", bad)
	}
}
