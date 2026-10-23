package eval

// TG-525 slice 2 — the LLM ORDERED-PATH JUDGE. It grades a session's ordered tool TRAJECTORY (the sequence of
// read-only tool calls the agent made while investigating) on a single 1..5 axis: was this a sensible,
// efficient investigation path, or a meandering / redundant one? Kept here (execphase-style), NOT in
// core/judge, so it never widens the investigate rubric or bumps RubricVersion; its output feeds only the
// REPORT-ONLY trajectory_grounded axis.
//
// THE JUDGE IS NOT THE AUTHORITY (docs/SYSTEM-MAP.md:106): the deterministic agent.TrajectoryVeto OVERRIDES
// this LLM grade in trajectoryScore — a path that loops or thrashes is forced to the worst score regardless of
// how leniently the LLM graded it, because a deterministic property of the agent's OWN actions outranks a
// model's opinion (INV-08). Slice 1 delivered the deterministic axis; this adds the richer LLM grade for the
// clean-path case, under the same veto.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/agent"
)

// trajectoryVerdict is the parsed LLM ordered-path grade.
type trajectoryVerdict struct {
	Score   int    `json:"score"`
	Comment string `json:"comment"`
}

// trajectoryJudgePrompt asks a judge to grade the ordered tool path. It shows the tool NAMES in order with a
// digested args token (never raw arg values — INV-13), so the judge grades the SHAPE of the investigation
// (did it gather the right KINDS of evidence in a sensible order, without redundant re-asks?), not specifics.
func trajectoryJudgePrompt(traj []agent.TrajectoryStep) string {
	var b strings.Builder
	b.WriteString("You are grading an AI incident-triage agent's INVESTIGATION TRAJECTORY: the ordered sequence of read-only tool calls it made while diagnosing an alert, before it decided. Grade the SHAPE of the investigation path, not its outcome.\n\n")
	b.WriteString("ORDERED TOOL CALLS (one per cycle; the args are digested to an opaque token, so judge the tool SEQUENCE, not the specific targets):\n")
	for i, s := range traj {
		key := s.ArgsKey
		if key == "" {
			key = "no-args"
		}
		fmt.Fprintf(&b, "%2d. %s [args:%s]\n", i+1, s.Tool, key)
	}
	b.WriteString("\nScore 1 (poor: aimless, redundant re-asks of the same call, ignores evidence it already gathered) to 5 (clean: gathers the right kinds of evidence in a sensible order and converges without waste).\n")
	b.WriteString(`Return ONLY a JSON object: {"score": <1-5>, "comment": "one line"}.`)
	return b.String()
}

// parseTrajectoryVerdict defensively extracts the {"score":N,"comment":...} JSON from a judge reply that may be
// wrapped in prose or code fences (the same tolerance parseExecJudgment applies, kept local so this module
// stays self-contained). ok=false when no valid 1..5 score can be recovered — the scorer then falls back to
// its deterministic default rather than inventing a grade.
func parseTrajectoryVerdict(raw string) (trajectoryVerdict, bool) {
	start := strings.IndexByte(raw, '{')
	end := strings.LastIndexByte(raw, '}')
	if start < 0 || end <= start {
		return trajectoryVerdict{}, false
	}
	var v trajectoryVerdict
	if err := json.Unmarshal([]byte(raw[start:end+1]), &v); err != nil {
		return trajectoryVerdict{}, false
	}
	if v.Score < 1 || v.Score > 5 {
		return trajectoryVerdict{}, false
	}
	return v, true
}
