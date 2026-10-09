package loadharness

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TG-80 P1-2 red-proof: a session that classifies (appears on the sessions page) but NEVER reaches a
// terminal status must FAIL the run, naming the last status seen — the harness used to stop at first
// visibility and would have scored it completed. Then the same shape with the stall removed must be green,
// and its latency must be POST→terminal (strictly longer than the classify latency alone).
func TestStalledNonTerminalRedProof(t *testing.T) {
	const runID = "redstall"
	stalled := ExpectedRef(runID, 4, 2)

	rig, err := StartRig(RigFaults{StallNonTerminalRef: stalled})
	if err != nil {
		t.Fatal(err)
	}
	cfg := rig.HarnessConfig()
	cfg.RunID, cfg.Runs, cfg.Levels = runID, 4, []int{4}
	cfg.SessionTimeout = 2 * time.Second
	rep := Run(context.Background(), cfg)
	rig.Close()

	if rep.ExitCode() == 0 {
		t.Fatalf("rig stalled %s at classified and the harness exited 0 — it scored first visibility as completion", stalled)
	}
	if rep.TotalFailed != 1 || rep.TotalCompleted != 3 {
		t.Fatalf("failed/completed = %d/%d, want 1/3; failures: %+v", rep.TotalFailed, rep.TotalCompleted, rep.Failures)
	}
	found := false
	for _, f := range rep.Failures {
		if f.Ref == stalled {
			found = true
			if !strings.Contains(f.Reason, "terminal") || !strings.Contains(f.Reason, `"classified"`) {
				t.Fatalf("the stall failure must name the terminal wait AND the last status seen, got %q", f.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("the stalled ref %s is not among the failures: %+v", stalled, rep.Failures)
	}

	// Restore: no stall → green, and every duration spans classify + terminal latency.
	rig2, err := StartRig(RigFaults{Latency: 60 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	cfg2 := rig2.HarnessConfig()
	cfg2.RunID, cfg2.Runs, cfg2.Levels = runID+"-ok", 4, []int{4}
	rep2 := Run(context.Background(), cfg2)
	rig2.Close()
	if rep2.ExitCode() != 0 {
		t.Fatalf("clean rig must be green, got failures %+v", rep2.Failures)
	}
	for _, lv := range rep2.Levels {
		if lv.P50Ms < 100 {
			t.Fatalf("level %d p50 = %dms — latency is still measured to first visibility, not to terminal (classify 60ms + terminal 60ms)", lv.Concurrency, lv.P50Ms)
		}
	}
}
