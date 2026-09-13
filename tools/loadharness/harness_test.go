package loadharness

// The harness's own oracles (TG-80 P1#2). The discipline is red-prove-then-restore: every invariant the
// harness claims to verify is broken ON THE RIG by a fault knob, the harness must go non-zero NAMING the
// failing ref, and the healthy rig must go green with percentiles emitted. A harness whose checks cannot
// fail proves nothing (the unfalsifiable-mutation-control lesson); a harness that can hang proves less
// (the hanging-oracle lesson — every wait here is deadline-bounded and the suite asserts it).

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestExpectedRefMatchesThePipeline pins the test-aiming helper against the REAL normalizer: the ref the
// rig's pipeline mints for a fixture host must be the ref ExpectedRef predicts. Every red-proof below
// aims its fault by ExpectedRef — if the module's am-<alertname>-<target> derivation drifted, the faults
// would aim at a ref that never exists, the rig would run healthy, and the red-proofs would fail loudly;
// this oracle turns that failure into a one-line diagnosis.
func TestExpectedRefMatchesThePipeline(t *testing.T) {
	rig, err := StartRig(RigFaults{})
	if err != nil {
		t.Fatal(err)
	}
	defer rig.Close()
	cfg := rig.HarnessConfig()
	cfg.RunID, cfg.Runs, cfg.Levels = "pin", 1, []int{1}
	rep := Run(context.Background(), cfg)
	if rep.ExitCode() != 0 {
		t.Fatalf("healthy rig, exit %d: %+v", rep.ExitCode(), rep.Failures)
	}
	// The single run's ref is not echoed in the report; re-read the spine to see what was minted.
	c := newClient(cfg)
	page, err := c.sessions(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	want := ExpectedRef("pin", 1, 0)
	found := false
	for _, s := range page.Sessions {
		if s.ExternalRef == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("pipeline minted no session with the predicted ref %q; page: %+v", want, page.Sessions)
	}
}

// TestHappyPathEightConcurrent is the zero-failure oracle: N=8 concurrent through the healthy rig ⇒
// exit 0, all invariants held, the duplicate probe ran mid-run and passed, and the percentiles are a
// real ordered distribution.
func TestHappyPathEightConcurrent(t *testing.T) {
	rig, err := StartRig(RigFaults{})
	if err != nil {
		t.Fatal(err)
	}
	defer rig.Close()
	cfg := rig.HarnessConfig()
	cfg.Runs, cfg.Levels = 8, []int{8}
	rep := Run(context.Background(), cfg)
	if rep.ExitCode() != 0 {
		t.Fatalf("exit %d, want 0; failures: %+v", rep.ExitCode(), rep.Failures)
	}
	if rep.TotalRuns != 8 || rep.TotalCompleted != 8 || rep.TotalFailed != 0 {
		t.Fatalf("runs/completed/failed = %d/%d/%d, want 8/8/0", rep.TotalRuns, rep.TotalCompleted, rep.TotalFailed)
	}
	lr := rep.Levels[0]
	if !lr.DuplicateProbe.Ran || !lr.DuplicateProbe.Passed {
		t.Fatalf("duplicate probe: %+v — must run mid-level and pass on a healthy rig", lr.DuplicateProbe)
	}
	if lr.MaxMs <= 0 || lr.P50Ms > lr.P95Ms || lr.P95Ms > lr.MaxMs {
		t.Fatalf("percentiles not an ordered distribution: p50=%d p95=%d max=%d", lr.P50Ms, lr.P95Ms, lr.MaxMs)
	}
	if lr.ThroughputPerSec <= 0 {
		t.Fatalf("throughput %v, want > 0", lr.ThroughputPerSec)
	}
	if !rep.QuietSpineChecked || rep.PopulationAfter-rep.PopulationBefore != 8 {
		t.Fatalf("quiet-spine: checked=%v delta=%d, want 8", rep.QuietSpineChecked, rep.PopulationAfter-rep.PopulationBefore)
	}
	var sb strings.Builder
	rep.WriteHuman(&sb)
	if !strings.Contains(sb.String(), "p50=") || !strings.Contains(sb.String(), "8 runs, 0 failures") {
		t.Fatalf("human output missing percentiles or the 0-failure line:\n%s", sb.String())
	}
}

// TestSweepRunsEveryLevel exercises the multi-level sweep plumbing: distinct fixture cohorts per level,
// each measured separately, one aggregated verdict.
func TestSweepRunsEveryLevel(t *testing.T) {
	rig, err := StartRig(RigFaults{})
	if err != nil {
		t.Fatal(err)
	}
	defer rig.Close()
	cfg := rig.HarnessConfig()
	cfg.Runs, cfg.Levels = 3, []int{1, 3}
	rep := Run(context.Background(), cfg)
	if rep.ExitCode() != 0 {
		t.Fatalf("exit %d, want 0; failures: %+v", rep.ExitCode(), rep.Failures)
	}
	if len(rep.Levels) != 2 || rep.TotalCompleted != 6 {
		t.Fatalf("levels=%d completed=%d, want 2 levels / 6 completed", len(rep.Levels), rep.TotalCompleted)
	}
	if delta := rep.PopulationAfter - rep.PopulationBefore; delta != 6 {
		t.Fatalf("population delta %d, want 6 (levels must not share a fixture cohort)", delta)
	}
}

// TestLostSessionRedProof breaks the rig so ONE session is minted but never persisted, and requires the
// harness to exit non-zero NAMING that ref as lost — then restores the rig and requires green with the
// same shape. This is the harness's core claim: a dropped session under load cannot pass.
func TestLostSessionRedProof(t *testing.T) {
	const runID = "redlost"
	dropped := ExpectedRef(runID, 4, 1)

	rig, err := StartRig(RigFaults{DropRef: dropped})
	if err != nil {
		t.Fatal(err)
	}
	cfg := rig.HarnessConfig()
	cfg.RunID, cfg.Runs, cfg.Levels = runID, 4, []int{4}
	cfg.SessionTimeout = 2 * time.Second // the lost ref times out here; siblings finish in tens of ms
	rep := Run(context.Background(), cfg)
	rig.Close()

	if rep.ExitCode() == 0 {
		t.Fatalf("rig dropped %s and the harness exited 0 — the loss check cannot fail", dropped)
	}
	if rep.TotalFailed != 1 || rep.TotalCompleted != 3 {
		t.Fatalf("failed/completed = %d/%d, want 1/3; failures: %+v", rep.TotalFailed, rep.TotalCompleted, rep.Failures)
	}
	found := false
	for _, f := range rep.Failures {
		if f.Ref == dropped && strings.Contains(f.Reason, "never appeared") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the lost ref %s is not named in the failures: %+v", dropped, rep.Failures)
	}

	// RESTORE: the identical run against a healthy rig is green (fresh rig ⇒ the refs are mintable again).
	rig2, err := StartRig(RigFaults{})
	if err != nil {
		t.Fatal(err)
	}
	defer rig2.Close()
	cfg2 := rig2.HarnessConfig()
	cfg2.RunID, cfg2.Runs, cfg2.Levels = runID, 4, []int{4}
	if rep2 := Run(context.Background(), cfg2); rep2.ExitCode() != 0 {
		t.Fatalf("restored rig still red: %+v", rep2.Failures)
	}
}

// TestDuplicateMintRedProof breaks reject-duplicate on the rig: the mid-run duplicate probe must catch
// the second minted session (a different workflow id for the same ref) and fail the run.
func TestDuplicateMintRedProof(t *testing.T) {
	rig, err := StartRig(RigFaults{BreakIdempotency: true})
	if err != nil {
		t.Fatal(err)
	}
	defer rig.Close()
	cfg := rig.HarnessConfig()
	cfg.Runs, cfg.Levels = 4, []int{4}
	rep := Run(context.Background(), cfg)
	if rep.ExitCode() == 0 {
		t.Fatal("rig double-mints on duplicate and the harness exited 0 — the idempotency probe cannot fail")
	}
	probe := rep.Levels[0].DuplicateProbe
	if !probe.Ran || probe.Passed {
		t.Fatalf("probe = %+v, want ran and FAILED", probe)
	}
	if !strings.Contains(probe.Reason, "SECOND session") {
		t.Fatalf("probe reason %q does not name the second mint", probe.Reason)
	}
}

// TestCrossContaminationRedProof breaks the rig so one ref's session lands with the WRONG host, and
// requires the harness to fail that run naming the ref — the "each session belongs to its own incident"
// invariant under concurrency.
func TestCrossContaminationRedProof(t *testing.T) {
	const runID = "redxcon"
	poisoned := ExpectedRef(runID, 3, 2)
	rig, err := StartRig(RigFaults{PoisonHostRef: poisoned})
	if err != nil {
		t.Fatal(err)
	}
	defer rig.Close()
	cfg := rig.HarnessConfig()
	cfg.RunID, cfg.Runs, cfg.Levels = runID, 3, []int{3}
	rep := Run(context.Background(), cfg)
	if rep.ExitCode() == 0 {
		t.Fatal("rig cross-contaminated a session and the harness exited 0 — the host-match check cannot fail")
	}
	found := false
	for _, f := range rep.Failures {
		if f.Ref == poisoned && strings.Contains(f.Reason, "cross-contamination") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the contaminated ref %s is not named: %+v", poisoned, rep.Failures)
	}
}

// TestWallClockCapBoundsTheRun proves the harness can time out but never hang (the hanging-oracle
// lesson): a rig whose sessions take 10s against a 500ms run cap must return promptly, non-zero.
func TestWallClockCapBoundsTheRun(t *testing.T) {
	rig, err := StartRig(RigFaults{Latency: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer rig.Close()
	cfg := rig.HarnessConfig()
	cfg.Runs, cfg.Levels = 2, []int{2}
	cfg.RunTimeout = 500 * time.Millisecond
	start := time.Now()
	rep := Run(context.Background(), cfg)
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("run took %s past a 500ms cap — the harness can hang", elapsed)
	}
	if rep.ExitCode() == 0 {
		t.Fatal("nothing completed and the harness exited 0")
	}
}

// TestPercentileNearestRank pins the published-number math: nearest-rank, never interpolated.
func TestPercentileNearestRank(t *testing.T) {
	durs := make([]time.Duration, 0, 10)
	for i := 1; i <= 10; i++ {
		durs = append(durs, time.Duration(i)*10*time.Millisecond)
	}
	if got := percentile(durs, 0.50); got != 50*time.Millisecond {
		t.Fatalf("p50 = %s, want 50ms", got)
	}
	if got := percentile(durs, 0.95); got != 100*time.Millisecond {
		t.Fatalf("p95 over 10 samples = %s, want 100ms (nearest rank 10)", got)
	}
	if got := percentile(durs, 1.0); got != 100*time.Millisecond {
		t.Fatalf("max = %s, want 100ms", got)
	}
	if got := percentile([]time.Duration{7 * time.Millisecond}, 0.95); got != 7*time.Millisecond {
		t.Fatalf("single sample p95 = %s, want 7ms", got)
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Fatalf("empty p50 = %s, want 0", got)
	}
}

// TestZeroRunReportsAreNeverGreen: a report that ran nothing (bad config, refused base URL) must not
// exit 0 — silence is not success.
func TestZeroRunReportsAreNeverGreen(t *testing.T) {
	rep := Run(context.Background(), Config{}) // no BaseURL — config refused before any request
	if rep.ExitCode() == 0 {
		t.Fatal("an empty run exited 0")
	}
	if len(rep.Failures) == 0 {
		t.Fatal("config refusal produced no named failure")
	}
}
