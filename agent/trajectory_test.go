package agent

import "testing"

func TestArgsKeyOrderIndependent(t *testing.T) {
	a := ArgsKey(map[string]string{"host": "web01", "rule": "Down"})
	b := ArgsKey(map[string]string{"rule": "Down", "host": "web01"})
	if a != b {
		t.Fatalf("ArgsKey must be order-independent: %q vs %q", a, b)
	}
	if ArgsKey(nil) != "" {
		t.Fatal("empty args must key to empty string")
	}
}

func TestTrajectoryVeto(t *testing.T) {
	step := func(tool string, args map[string]string) TrajectoryStep {
		return TrajectoryStep{Tool: tool, ArgsKey: ArgsKey(args)}
	}
	getLogs := step("get-logs", map[string]string{"host": "web01"})

	// three identical consecutive calls → loop veto
	if veto, reason := TrajectoryVeto([]TrajectoryStep{getLogs, getLogs, getLogs}); !veto || reason == "" {
		t.Fatalf("three identical consecutive calls must veto, got %v %q", veto, reason)
	}
	// distinct calls do not veto
	traj := []TrajectoryStep{
		step("get-logs", map[string]string{"host": "web01"}),
		step("get-metrics", map[string]string{"host": "web01"}),
		step("get-logs", map[string]string{"host": "db01"}),
	}
	if veto, _ := TrajectoryVeto(traj); veto {
		t.Fatalf("distinct calls must not veto: %+v", traj)
	}
	// oscillation: A B A B A B A → each of A,B recurs enough to trip the thrash threshold
	a, b := step("a", nil), step("b", nil)
	thrash := []TrajectoryStep{a, b, a, b, a, b, a}
	if veto, reason := TrajectoryVeto(thrash); !veto || reason == "" {
		t.Fatalf("oscillation must veto as thrash, got %v %q", veto, reason)
	}
	// empty / short trajectories never veto
	if veto, _ := TrajectoryVeto(nil); veto {
		t.Fatal("empty trajectory must not veto")
	}
	if veto, _ := TrajectoryVeto([]TrajectoryStep{getLogs, getLogs}); veto {
		t.Fatal("two identical calls (below LoopThreshold) must not veto")
	}
}
