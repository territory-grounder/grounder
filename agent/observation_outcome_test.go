package agent

import (
	"context"
	"strings"
	"testing"
)

// failedLookupOutput is the shape that makes this defect dangerous: a lookup that FAILED, whose text reads
// exactly like a successful "nothing found" answer. get-tracker-history returns this live
// (modules/tracker/trackerhistory) — an unreadable corpus is Success=false with prose, and "no prior
// incidents" is the difference between "this fault is novel" and "I could not look".
const failedLookupOutput = "tracker history for web01: no prior incidents found in the shared tracker."

// failedLookupTool is a read-only tool whose call FAILS while returning fact-shaped text.
type failedLookupTool struct{}

func (failedLookupTool) Name() string   { return "get-tracker-history" }
func (failedLookupTool) ReadOnly() bool { return true }
func (failedLookupTool) Invoke(_ context.Context, _ map[string]string) (ToolResult, error) {
	return ToolResult{ID: "trk-9", Tool: "get-tracker-history", Output: failedLookupOutput, Success: false}, nil
}

// proposeCitingFailed cites trk-9 — an id the orchestrator really did capture, so the in-loop citation gate
// (id MEMBERSHIP only) admits it. That is the point: the gate cannot see the failure, so the model's own
// input is the only place the failure can be surfaced before the citation is made.
const proposeCitingFailed = `{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-1","target":"web01","op_class":"restart-service","op":"restart","reversible":true,"confidence":0.85,"evidence_ids":["trk-9"]}}`

// TestFailedObservationIsMarkedFAILEDInThePromptTheModelReads is the TG-199 oracle: the OBSERVATION envelope
// the model reads carries the orchestrator's succeeded/failed verdict, so a failed lookup cannot be mistaken
// for a reading of the estate — while the evidence id, the anchor everything downstream binds against, stays
// verbatim.
//
// KILLING MUTATION (executed 2026-08-04): in observationEnvelope, delete the TOOL_OUTCOME line and return the
// pre-TG-199 body `fmt.Sprintf("OBSERVATION[%s]: %s", tr.ID, screened)`. RED:
//
//	"a proposal can rest on a FAILED lookup: get-tracker-history FAILED, but the model was shown
//	 OBSERVATION[trk-9] with no outcome marker ..."
//
// Restored → green.
func TestFailedObservationIsMarkedFAILEDInThePromptTheModelReads(t *testing.T) {
	m := &promptCapture{responses: []string{
		`{"action":"tool","tool":"get-tracker-history","args":{"host":"web01"},"confidence":0.8}`, // trk-9, FAILED
		`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,            // tr-1, succeeded
		proposeCitingFailed,
	}}
	ts := NewReadOnlyToolSet()
	if err := ts.Register(failedLookupTool{}); err != nil {
		t.Fatalf("register failedLookupTool: %v", err)
	}
	if err := ts.Register(readTool{}); err != nil {
		t.Fatalf("register readTool: %v", err)
	}
	ag := &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "primary", User: "t"}
	res, err := ag.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// VACUITY FLOOR. Everything below scans the prompt the model was handed; a scan over an empty or
	// observation-less corpus would "pass" for free. Prove the corpus is the one this test means to grade
	// BEFORE grading it — including that the failing fixture really did fail (a fixture that silently
	// flipped to Success=true would make the FAILED assertion unreachable rather than false).
	joined := strings.Join(m.seenUser, "\n")
	if n := strings.Count(joined, "OBSERVATION["); n < 2 {
		t.Fatalf("vacuity floor: the scanned prompt must contain both observations, found %d in:\n%s", n, joined)
	}
	if len(res.ToolResults) != 2 || res.ToolResults[0].ID != "trk-9" || res.ToolResults[1].ID != "tr-1" {
		t.Fatalf("both tool results must be captured with their real ids, got %+v", res.ToolResults)
	}
	if res.ToolResults[0].Success || !res.ToolResults[1].Success {
		t.Fatalf("fixture drift: trk-9 must be the FAILED result and tr-1 the successful one, got %+v", res.ToolResults)
	}

	failIdx := strings.Index(joined, "TOOL_OUTCOME[trk-9]: FAILED")
	if failIdx < 0 {
		t.Fatalf("a proposal can rest on a FAILED lookup: get-tracker-history FAILED, but the model was shown "+
			"OBSERVATION[trk-9] with no outcome marker — %q reads exactly like a real reading, so the model "+
			"cites it as evidence and the fix is justified by a lookup that never landed (it is then refused "+
			"at the actuation evidence gate with no feedback, burning the session). Prompt:\n%s",
			failedLookupOutput, joined)
	}
	obsIdx := strings.Index(joined, "OBSERVATION[trk-9]:")
	if obsIdx < 0 {
		t.Fatalf("the OBSERVATION[<id>] envelope is the evidence anchor and must survive verbatim, got:\n%s", joined)
	}
	// The orchestrator's verdict must LEAD the untrusted payload: a tool result is attacker-influenceable
	// (INV-08), and trusted text placed after it can be argued with by the text above it.
	if failIdx > obsIdx {
		t.Fatalf("the TOOL_OUTCOME verdict must PRECEDE the untrusted observation payload it describes "+
			"(verdict at %d, observation at %d), got:\n%s", failIdx, obsIdx, joined)
	}
	// A successful call is marked too. Without this, "no marker" would be the only success signal — and every
	// future path that forgets the marker would silently read as SUCCEEDED, which is the defect again.
	if !strings.Contains(joined, "TOOL_OUTCOME[tr-1]: SUCCEEDED") {
		t.Fatalf("a SUCCEEDED call must be marked explicitly, or an absent marker reads as success, got:\n%s", joined)
	}
	// The screened payload and its id envelope are untouched — surfacing the outcome must not disturb the
	// anchor the citation gate, INV-11 binding and the console citation all resolve against (REQ-1012).
	if !strings.Contains(joined, "OBSERVATION[tr-1]: nginx is down") {
		t.Fatalf("the observation envelope + payload must pass through unchanged, got:\n%s", joined)
	}

	// The in-loop citation gate is deliberately UNCHANGED by TG-199 — it checks id membership, so it still
	// admits this proposal. What changed is that the model now reads the failure BEFORE it cites; the
	// mechanical unbinding of a failed citation stays where it lives (buildEvidence / actuateEvidence →
	// core/actuate's evidence gate). Asserted so a future change to the gate is a deliberate one.
	if res.Outcome != OutcomeProposed {
		t.Fatalf("the id-membership citation gate is unchanged here; want proposed, got %s (%s)", res.Outcome, res.Reason)
	}
	// The tracer row must not describe a failed lookup as a plain observation either.
	var sawFailedStep bool
	for _, st := range res.Steps {
		if strings.Contains(st.Observation, "trk-9") && strings.Contains(st.Observation, "FAILED") {
			sawFailedStep = true
		}
	}
	if !sawFailedStep {
		t.Fatalf("the cycle transcript must record that trk-9 was a FAILED call, or the operator and the judge "+
			"see the same bare 'observed trk-9' the model used to see, got %+v", res.Steps)
	}
}

// TestPreambleTeachesTheOutcomeMarker: a marker the model was never taught to read is decoration. The
// preamble must name the exact token the envelope emits and state the duty that follows from it — a FAILED
// observation is not evidence, so it is never cited.
//
// KILLING MUTATION (executed 2026-08-04): revert the two preamble edits (the TOOL_OUTCOME sentence in the
// tool bullet and the "Cite only ids whose TOOL_OUTCOME was SUCCEEDED" clause in the grounding paragraph),
// leaving observationEnvelope stamping a marker the model was never told about. RED on the first token.
// Restored → green.
func TestPreambleTeachesTheOutcomeMarker(t *testing.T) {
	content := protocolPreamble(nil, "", "").Content
	for _, want := range []string{
		"TOOL_OUTCOME[<id>]: SUCCEEDED|FAILED",           // the exact vocabulary observationEnvelope emits
		"never cite a FAILED id",                         // the duty that makes the marker load-bearing
		"Cite only ids whose TOOL_OUTCOME was SUCCEEDED", // the grounding contract names it too
		"spends one of your limited cycles",              // retrying a failed call is not free
	} {
		if !strings.Contains(content, want) {
			t.Errorf("the preamble must teach %q — the loop stamps the outcome on every observation, and a "+
				"marker the model was never taught to read leaves it citing failed lookups exactly as before", want)
		}
	}
}

// alwaysFailsTool never succeeds — the "host unreachable" tool. Each call gets its own id so a repeat is
// visible in the captured results.
type alwaysFailsTool struct{ calls int }

func (t *alwaysFailsTool) Name() string   { return "probe-host" }
func (t *alwaysFailsTool) ReadOnly() bool { return true }
func (t *alwaysFailsTool) Invoke(_ context.Context, _ map[string]string) (ToolResult, error) {
	t.calls++
	return ToolResult{ID: "probe-" + itoa(t.calls), Tool: "probe-host", Output: "host unreachable", Success: false}, nil
}

// TestFailedObservationStillCostsACycle guards the failure mode TG-199 CREATES: a model that can finally see
// "FAILED" may just retry. It cannot retry forever — a failed observation walks the SAME path as a successful
// one (captured, recorded as a TrajectoryStep, cycle counter advanced), so an always-failing tool runs the
// budget out and hands off, and repeating the IDENTICAL failing call is vetoed even sooner.
//
// KILLING MUTATION (executed 2026-08-04): refund the cycle a failed observation spent — the tempting
// "a lookup that failed shouldn't cost the agent anything" tweak. In the tool branch, after
// investigatedThisCycle is set, add `if !tr.Success { cycle--; continue }`. RED: "an always-failing tool
// must run the cycle budget out and hand off, got stop" — the run never reaches the budget because every
// retry is free. Restored → green.
func TestFailedObservationStillCostsACycle(t *testing.T) {
	// Distinct args per cycle, so the CYCLE budget — not the trajectory veto — is what bounds this run.
	var distinct []string
	for i := 0; i < 8; i++ {
		distinct = append(distinct, `{"action":"tool","tool":"probe-host","args":{"host":"h`+itoa(i)+`"},"confidence":0.8}`)
	}
	ts := NewReadOnlyToolSet()
	if err := ts.Register(&alwaysFailsTool{}); err != nil {
		t.Fatalf("register alwaysFailsTool: %v", err)
	}
	ag := &Agent{Model: &scriptedModel{responses: distinct}, Tools: ts,
		Limits: Limits{HandoffPoll: 100, HandoffHalt: 4}, ModelName: "primary", User: "t"}
	res, err := ag.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != OutcomeHardHalt {
		t.Fatalf("an always-failing tool must run the cycle budget out and hand off, got %s (%s) — if a FAILED "+
			"observation is free, the model that can now READ \"FAILED\" retries without limit: the estate is "+
			"never triaged, the incident never reaches a human, and the session burns model spend until "+
			"something else stops it", res.Outcome, res.Reason)
	}
	if res.Cycles != 4 {
		t.Fatalf("every FAILED observation must cost exactly one cycle — 4 cycles of budget, got %d", res.Cycles)
	}
	if len(res.ToolResults) != 4 {
		t.Fatalf("each failed call must still be captured as an observation, got %d: %+v", len(res.ToolResults), res.ToolResults)
	}
	for _, tr := range res.ToolResults {
		if tr.Success {
			t.Fatalf("fixture drift: every probe-host result must be a failure, got %+v", res.ToolResults)
		}
	}

	// The tighter bound: re-issuing the IDENTICAL failing call — the literal "it said FAILED, try again"
	// reflex — is vetoed by the deterministic trajectory check well before the budget.
	same := []string{}
	for i := 0; i < 6; i++ {
		same = append(same, `{"action":"tool","tool":"probe-host","args":{"host":"web01"},"confidence":0.8}`)
	}
	ts2 := NewReadOnlyToolSet()
	if err := ts2.Register(&alwaysFailsTool{}); err != nil {
		t.Fatalf("register alwaysFailsTool: %v", err)
	}
	ag2 := &Agent{Model: &scriptedModel{responses: same}, Tools: ts2,
		Limits: Limits{HandoffPoll: 100, HandoffHalt: 10}, ModelName: "primary", User: "t"}
	res2, err := ag2.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res2.Outcome != OutcomeStop || !strings.Contains(res2.Reason, "trajectory veto") {
		t.Fatalf("repeating the identical FAILED call must trip the trajectory veto, got %s (%s)", res2.Outcome, res2.Reason)
	}
	if res2.Cycles > LoopThreshold {
		t.Fatalf("the veto must fire at the loop threshold (%d), not after the budget; got %d cycles", LoopThreshold, res2.Cycles)
	}
}
