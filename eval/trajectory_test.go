package eval

import (
	"testing"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/judge"
)

func trajStep(tool, args string) agent.TrajectoryStep {
	return agent.TrajectoryStep{Tool: tool, ArgsKey: args}
}

func TestTrajectoryScore(t *testing.T) {
	if _, ok := trajectoryScore(Session{}); ok {
		t.Fatal("an empty trajectory must be N/A (ok=false), never scored")
	}
	cleanTraj := []agent.TrajectoryStep{trajStep("get_logs", "h=a"), trajStep("get_metrics", "h=a"), trajStep("check", "h=b")}
	// Clean path, NO LLM grade -> deterministic fallback to 5 (the slice-1 read).
	if v, ok := trajectoryScore(Session{Trajectory: cleanTraj}); !ok || v != 5 {
		t.Fatalf("a clean trajectory with no LLM grade must default to 5, got %d ok=%v", v, ok)
	}
	// Clean path WITH an LLM ordered-path grade of 3 -> the judge's grade (slice 2).
	if v, ok := trajectoryScore(Session{Trajectory: cleanTraj, TrajectoryJudgeScore: 3}); !ok || v != 3 {
		t.Fatalf("a clean trajectory must take the LLM ordered-path grade, got %d ok=%v", v, ok)
	}
	// THE CORE SLICE-2 GUARANTEE: a looping path -> the deterministic veto forces 1, OVERRIDING a lenient LLM
	// grade of 5. The judge is not the authority.
	loop := []agent.TrajectoryStep{trajStep("get_logs", "h=a"), trajStep("get_logs", "h=a"), trajStep("get_logs", "h=a")}
	if v, ok := trajectoryScore(Session{Trajectory: loop, TrajectoryJudgeScore: 5}); !ok || v != 1 {
		t.Fatalf("the deterministic veto must OVERRIDE the LLM grade (want 1 despite judge=5), got %d ok=%v", v, ok)
	}
}

func TestParseTrajectoryVerdict(t *testing.T) {
	if v, ok := parseTrajectoryVerdict(`{"score": 4, "comment": "sensible order"}`); !ok || v.Score != 4 {
		t.Fatalf("must parse a clean verdict, got %+v ok=%v", v, ok)
	}
	if v, ok := parseTrajectoryVerdict("Grade:\n```json\n{\"score\": 2, \"comment\": \"redundant re-asks\"}\n```"); !ok || v.Score != 2 {
		t.Fatalf("must extract the JSON from wrapped prose/fences, got %+v ok=%v", v, ok)
	}
	if _, ok := parseTrajectoryVerdict(`{"score": 9}`); ok {
		t.Fatal("an out-of-range score must be rejected (ok=false) — the scorer falls back, never invents a grade")
	}
	if _, ok := parseTrajectoryVerdict("I cannot grade this path."); ok {
		t.Fatal("a reply with no JSON object must be rejected (ok=false)")
	}
}

func TestTrajectoryGroundedIsReportedButNeverGated(t *testing.T) {
	sessions := []Session{
		{Ref: "a", Trajectory: []agent.TrajectoryStep{trajStep("x", "1"), trajStep("y", "2")}},                     // clean -> 5
		{Ref: "b", Trajectory: []agent.TrajectoryStep{trajStep("z", "1"), trajStep("z", "1"), trajStep("z", "1")}}, // loop  -> 1
		{Ref: "c"}, // no trajectory -> N/A, omitted
	}
	sc := Aggregate(sessions, nil)
	if mean, ok := sc.DimMeans[dimTrajectoryGrounded]; !ok || mean != 3.0 {
		t.Fatalf("trajectory_grounded mean must be (5+1)/2 = 3.0 over the two scored sessions, got %v ok=%v", mean, ok)
	}
	if n := sc.DimSamples[dimTrajectoryGrounded]; n != 2 {
		t.Fatalf("the N/A session must be omitted from the sample count (want 2), got %d", n)
	}
	// The load-bearing property (TG-525): report-only — NOT one of the gated judge.Dimensions, so it can never
	// bar a merge. (gate.Dimensions == judge.Dimensions; gate.Compare reasons only over that set.)
	for _, d := range judge.Dimensions {
		if d == dimTrajectoryGrounded {
			t.Fatal("trajectory_grounded must NOT be in the gated judge.Dimensions set — it is report-only")
		}
	}
}
