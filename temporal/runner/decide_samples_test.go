package runner

import (
	"context"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
)

// TG-46 — the forced-decision self-consistency width and its provenance, activity-side.

// decideSamplesFor gates on the SAME two written-out conditions as the model-tier floor — a
// DEEP_INVESTIGATION topology, or a CRITICAL severity — and the knob is a dial-back for that set only:
// it can narrow the gated width to 1 and can never widen the ungated classes past their single call.
// Garbage never disarms: the default 3 IS the armed behavior.
func TestDecideSamplesForGateAndKnob(t *testing.T) {
	crit := ingest.IncidentEnvelope{ExternalRef: "x", Host: "h", AlertRule: "r", Severity: ingest.SeverityCritical}
	warn := ingest.IncidentEnvelope{ExternalRef: "x", Host: "h", AlertRule: "r", Severity: ingest.SeverityWarning}
	deep := string(execclass.DeepInvestigation)
	std := string(execclass.StandardAgent)
	fast := string(execclass.FastAgent)

	for _, tc := range []struct {
		name string
		env  ingest.IncidentEnvelope
		cls  string
		knob string
		want int
	}{
		{"deep topology, default", warn, deep, "", 3},
		{"critical severity, unclassified (legacy fallback)", crit, "", "", 3},
		{"critical severity binds even on a non-deep class", crit, std, "", 3},
		{"ordinary standard session", warn, std, "", 1},
		{"fast class", warn, fast, "", 1},
		{"unclassified warning (legacy fallback)", warn, "", "", 1},
		{"dial-back to 1 disarms the gated set", warn, deep, "1", 1},
		{"an explicit 2 is honored", warn, deep, "2", 2},
		{"values above the ceiling clamp", warn, deep, "9", 5},
		{"garbage never disarms the default", warn, deep, "three", 3},
		{"a non-positive value never disarms the default", warn, deep, "0", 3},
		{"the knob cannot widen an ungated class", warn, std, "4", 1},
	} {
		t.Setenv("TG_DECIDE_SAMPLES", tc.knob)
		if got := decideSamplesFor(tc.env, tc.cls); got != tc.want {
			t.Fatalf("%s: decideSamplesFor = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// dvrProposeGroundedHi shares proposeGroundedWeb01's key (propose/restart-service on web01, citing tr-1)
// at a higher confidence, so a wired 2-1 majority has a distinguishable best statement.
const dvrProposeGroundedHi = `{"action":"propose","confidence":0.9,"proposal":{"external_ref":"TG-1","target":"web01","op_class":"restart-service","op":"restart","reversible":true,"confidence":0.9,"evidence_ids":["tr-1"]}}`

// dvrStopGrounded is a grounded stand-down decide sample (cites tr-1).
const dvrStopGrounded = `{"action":"stop","confidence":0.8,"reason":"pressure already cleared; no action warranted","evidence_ids":["tr-1"]}`

// dvrProposeOtherClass is a grounded proposal in a DIFFERENT op_class — the third corner of a 3-way split.
const dvrProposeOtherClass = `{"action":"propose","confidence":0.8,"proposal":{"external_ref":"TG-1","target":"db01","op_class":"resize-disk","op":"resize","reversible":true,"confidence":0.8,"evidence_ids":["tr-1"]}}`

// A critical-severity session that reaches the forced decision draws 3 samples, and the DURABLE record
// says so on the provenance channel: the summary note (drawn-of-requested + disagreement), one structured
// note per sample, and NO split marker for a clean 2-1 majority. Wired through the real workflow so the
// activity boundary and the TriageRow projection are both on the hook.
func TestTriageRecordCarriesDecideSampleProvenance(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	script := append(distinctToolScript(5), proposeGroundedWeb01, dvrStopGrounded, dvrProposeGroundedHi)
	deps := testDeps(script...)
	var recorded []judge.TriageRow
	deps.TriageRecord = func(_ context.Context, row judge.TriageRow) error { recorded = append(recorded, row); return nil }
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-46-vote", Host: "web01", AlertRule: "HostDown",
		Severity: ingest.SeverityCritical, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	if len(recorded) != 1 {
		t.Fatalf("expected exactly one durable triage row, got %d", len(recorded))
	}
	row := recorded[0]
	if !row.Proposed {
		t.Fatalf("setup: the 2-1 majority must land the proposal, got outcome %q", row.Outcome)
	}
	if !hasNote(row.SkillLoads, "decide-samples:3-of-3:disagreement:1") {
		t.Fatalf("the record must carry the draw summary, skill_loads=%v", row.SkillLoads)
	}
	if !hasNote(row.SkillLoads, "decide-sample:propose:restart-service:web01") ||
		!hasNote(row.SkillLoads, "decide-sample:stop::") {
		t.Fatalf("every sample's structured vote must be recorded, skill_loads=%v", row.SkillLoads)
	}
	if hasNote(row.SkillLoads, "decide-samples:SPLIT") {
		t.Fatalf("a clean majority must not carry the split marker, skill_loads=%v", row.SkillLoads)
	}
}

// A 3-way split resolves to the conservative stand-down AND leaves the LOUD marker beside the sample
// notes — the disagreement an operator reading the record must not have to reconstruct.
func TestTriageRecordCarriesDecideSplitLoudMarker(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	script := append(distinctToolScript(5), dvrProposeGroundedHi, dvrStopGrounded, dvrProposeOtherClass)
	deps := testDeps(script...)
	var recorded []judge.TriageRow
	deps.TriageRecord = func(_ context.Context, row judge.TriageRow) error { recorded = append(recorded, row); return nil }
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-46-split", Host: "web01", AlertRule: "HostDown",
		Severity: ingest.SeverityCritical, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	if len(recorded) != 1 {
		t.Fatalf("expected exactly one durable triage row, got %d", len(recorded))
	}
	row := recorded[0]
	if row.Proposed || row.Outcome != "no-proposal:stop" {
		t.Fatalf("the split must resolve to the stand-down, got proposed=%v outcome=%q", row.Proposed, row.Outcome)
	}
	if !hasNote(row.SkillLoads, "decide-samples:SPLIT:conservative-resolution") ||
		!hasNote(row.SkillLoads, "decide-samples:3-of-3:disagreement:2") {
		t.Fatalf("a split must be recorded LOUDLY, skill_loads=%v", row.SkillLoads)
	}
}

// The control: an ordinary (warning, standard-class) session keeps the single-call decide and records NO
// decide-sample provenance at all — the gated arm's record must stay distinguishable from the default's.
func TestNonGatedSessionRecordsNoDecideSampleNotes(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	script := append(distinctToolScript(5), proposeGroundedWeb01)
	deps := testDeps(script...)
	var recorded []judge.TriageRow
	deps.TriageRecord = func(_ context.Context, row judge.TriageRow) error { recorded = append(recorded, row); return nil }
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-46-ctrl", Host: "web01", AlertRule: "HostDown",
		Severity: ingest.SeverityWarning, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	if len(recorded) != 1 {
		t.Fatalf("expected exactly one durable triage row, got %d", len(recorded))
	}
	row := recorded[0]
	if !row.Proposed {
		t.Fatalf("setup: the nudged single-call decide must land its proposal, got %q", row.Outcome)
	}
	if hasNote(row.SkillLoads, "decide-samples:") || hasNote(row.SkillLoads, "decide-sample:") {
		t.Fatalf("an ungated session must record no decide-sample provenance, skill_loads=%v", row.SkillLoads)
	}
}
