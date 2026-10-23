package safety

// TG-473 (epic TG-114 leaf 4) — the safety-prose HASH-LOCK, as an EXECUTABLE membership oracle.
//
// The epic's leaf asked for two properties on safety-critical prose (the never-auto floor slugs, the
// stateful/destructive pattern law here, the injection screen in core/screen): (1) hash-locked so it cannot
// drift from its spec without an owner-trailered restamp, and (2) structurally outside the prose flywheel.
// Property (2) is enforced BY ABSENCE (ADR-0017: safety prose is deliberately NOT an artifact class — the
// store's closed class enum refuses it, pinned by the class-vocabulary oracles) and property (1) by
// spec/.lockstep.lock membership. The proposed externalization to data files was REJECTED at review
// (recorded on TG-473): both properties already hold, the pattern law's rationale lives in the comments
// beside each regex (splitting law from its reasons is a review regression), and a new loader panic path in
// the safety core is risk without a failing oracle.
//
// This test makes property (1) FALSIFIABLE instead of asserted: the two prose-law files must be members of
// the lockstep set. KILLING MUTATION (executed at review): delete the core/safety/safety.go entry from
// spec/.lockstep.lock — this reddens naming the missing member; restore ⇒ green. Without this oracle,
// lock-set membership was enforced only by nobody editing the JSON.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSafetyProseFilesAreLockstepMembers(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "spec", ".lockstep.lock"))
	if err != nil {
		t.Fatalf("read lockstep lock: %v", err)
	}
	var lock struct {
		Files []struct {
			Path string `json:"path"`
			Spec string `json:"spec"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatalf("parse lockstep lock: %v", err)
	}
	want := map[string]bool{
		"core/safety/safety.go": false, // the floor slugs + stateful/destructive pattern law
		"core/screen/screen.go": false, // the injection screen patterns
	}
	for _, f := range lock.Files {
		if _, ok := want[f.Path]; ok {
			want[f.Path] = true
			if f.Spec == "" {
				t.Errorf("%s is in the lock set with no owning spec", f.Path)
			}
		}
	}
	for path, seen := range want {
		if !seen {
			t.Errorf("%s is NOT in spec/.lockstep.lock — the safety-prose hash-lock (TG-473) has been "+
				"removed; restoring it requires the owning spec's prose change + an owner-trailered restamp", path)
		}
	}
}
