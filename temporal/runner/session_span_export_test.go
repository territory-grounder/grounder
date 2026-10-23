package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/ingest"
)

// spanRecorder is a trace sink that captures what the investigate activity ships.
type spanRecorder struct {
	sessionID string
	spans     []string
	calls     int
	err       error
}

func (s *spanRecorder) ExportSpans(_ context.Context, sessionID string, spans []string) error {
	s.calls++
	s.sessionID, s.spans = sessionID, spans
	return s.err
}

// usageModel is a scripted completer that ALSO reports provider usage — the shape the real gateway has
// since TG-44. It lets the wiring oracle assert that a REPORTED token count reaches the exported trace.
type usageModel struct {
	responses []string
	n         int
	perCall   model.Usage
}

func (u *usageModel) Complete(ctx context.Context, user, m string, msgs []model.Message) (string, error) {
	out, _, err := u.CompleteWithUsage(ctx, user, m, msgs)
	return out, err
}

func (u *usageModel) CompleteWithUsage(_ context.Context, _, _ string, _ []model.Message) (string, model.Usage, error) {
	out := ""
	if u.n < len(u.responses) {
		out = u.responses[u.n]
	}
	u.n++
	return out, u.perCall, nil
}

// TestInvestigateExportsSessionSpans is the TG-44 WIRING oracle.
//
// openobserve.ExportSpans has existed since spec/008 with tracing default-ON and had NO caller anywhere in
// the tree — the module's own descriptor documented it ("no worker path calls it today, so no traces
// ship"). This test is what makes the caller real: it drives the actual investigate activity and asserts a
// trace left the building, keyed by external_ref and carrying the cost/latency facts.
//
// KILLING MUTATION (EXECUTED 2026-08-04): delete the `if a.D.SessionSpans != nil { ... }` export block from
// InvestigateActivity — i.e. return to the pre-TG-44 world where the method exists and nothing calls it.
// This test fails with
//
//	the investigation completed and NO trace was exported — ExportSpans is unwired again,
//	which is the exact TG-44 defect (a capability nobody invokes is not a capability)
//
// Block restored, green.
func TestInvestigateExportsSessionSpans(t *testing.T) {
	sink := &spanRecorder{}
	deps := testDeps()
	deps.Model = &usageModel{
		responses: []string{investigateToolCall, proposeCitingToolResult},
		perCall:   model.Usage{PromptTokens: 1603, CompletionTokens: 4, TotalTokens: 1607, Measured: true},
	}
	deps.SessionSpans = sink

	res, err := NewActivities(deps).InvestigateActivity(context.Background(),
		ingest.IncidentEnvelope{ExternalRef: "TG-1", Host: "web01", AlertRule: "NginxDown"}, "", ClusterMemberContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Proposed {
		t.Fatalf("scenario should propose: %+v", res)
	}

	if sink.calls != 1 {
		t.Fatalf("the investigation completed and NO trace was exported (calls=%d) — ExportSpans is unwired "+
			"again, which is the exact TG-44 defect (a capability nobody invokes is not a capability)", sink.calls)
	}
	if sink.sessionID != "TG-1" {
		t.Fatalf("trace keyed by %q, want the external_ref TG-1 — an unkeyed trajectory cannot be joined to "+
			"any other record of the session", sink.sessionID)
	}
	if len(sink.spans) < 2 {
		t.Fatalf("exported %d spans, want a summary plus at least one cycle: %v", len(sink.spans), sink.spans)
	}
	summary := sink.spans[0]
	for _, want := range []string{
		"name=session.investigate", `outcome=proposed`, "tool_calls=1", "tool_errors=0",
		"tokens_source=measured", "tokens_total=3214", // two completions x 1607 reported tokens
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary span missing %q — got %q", want, summary)
		}
	}
	// The registered tool must appear by name; the transcript is the point of the trace.
	joined := strings.Join(sink.spans, "\n")
	if !strings.Contains(joined, "tool=get-logs") {
		t.Errorf("no cycle span names the tool that was actually called:\n%s", joined)
	}
}

// TestInvestigateSurvivesATraceExportFailure. Observability must never be able to break an investigation —
// the same contract the agent_step and agent_step_evidence sinks hold.
func TestInvestigateSurvivesATraceExportFailure(t *testing.T) {
	sink := &spanRecorder{err: errors.New("openobserve: POST /v1/traces: status 401")}
	deps := testDeps(investigateToolCall, proposeCitingToolResult)
	deps.SessionSpans = sink

	res, err := NewActivities(deps).InvestigateActivity(context.Background(),
		ingest.IncidentEnvelope{ExternalRef: "TG-1", Host: "web01", AlertRule: "NginxDown"}, "", ClusterMemberContext{})
	if err != nil {
		t.Fatalf("a failed trace export must NOT fail the investigation: %v", err)
	}
	if !res.Proposed {
		t.Fatalf("a failed trace export must not change the outcome: %+v", res)
	}
	if sink.calls != 1 {
		t.Fatalf("the export was not even attempted (calls=%d)", sink.calls)
	}
}

// TestInvestigateWithNoTraceSinkIsUnchanged: the safe default. A deployment with no trace store configured
// must behave exactly as it did before this change.
func TestInvestigateWithNoTraceSinkIsUnchanged(t *testing.T) {
	deps := testDeps(investigateToolCall, proposeCitingToolResult)
	deps.SessionSpans = nil
	res, err := NewActivities(deps).InvestigateActivity(context.Background(),
		ingest.IncidentEnvelope{ExternalRef: "TG-1", Host: "web01", AlertRule: "NginxDown"}, "", ClusterMemberContext{})
	if err != nil {
		t.Fatalf("nil sink must be a no-op: %v", err)
	}
	if !res.Proposed {
		t.Fatalf("nil sink must not change the outcome: %+v", res)
	}
}

// TestUnmeasurableSessionExportsUnknownRatherThanZero: the scripted model in the rest of the suite cannot
// report usage, so the exported trace must say tokens_source=unknown. Publishing tokens_total=0 for a
// session that made two model calls would teach a dashboard that investigations are free.
func TestUnmeasurableSessionExportsUnknownRatherThanZero(t *testing.T) {
	sink := &spanRecorder{}
	deps := testDeps(investigateToolCall, proposeCitingToolResult) // plain scriptedModel: no usage seam
	deps.SessionSpans = sink

	if _, err := NewActivities(deps).InvestigateActivity(context.Background(),
		ingest.IncidentEnvelope{ExternalRef: "TG-1", Host: "web01", AlertRule: "NginxDown"}, "", ClusterMemberContext{}); err != nil {
		t.Fatal(err)
	}
	if sink.calls != 1 {
		t.Fatalf("no trace exported (calls=%d)", sink.calls)
	}
	summary := sink.spans[0]
	if !strings.Contains(summary, "tokens_source=unknown") {
		t.Fatalf("summary %q must declare tokens_source=unknown when no call reported usage", summary)
	}
	if strings.Contains(summary, "tokens_total=") {
		t.Fatalf("summary %q published a token total it never measured", summary)
	}
	if !strings.Contains(summary, "model_calls=2") {
		t.Fatalf("summary %q must still report that 2 model calls were MADE, so 'unknown' is distinguishable "+
			"from 'no model was called'", summary)
	}
}
