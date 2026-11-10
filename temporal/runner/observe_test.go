package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/core/observe"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// investigateToolCall drives one read-only tool call (the testDeps get-logs tool) before the proposal, so
// the recorded loop has a tool-call count > 0. proposeCitingToolResult then cites that captured observation
// (tr-1) so the citation gate accepts it and the loop terminates OutcomeProposed.
const (
	investigateToolCall     = `{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`
	proposeCitingToolResult = `{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-1","target":"web01","op_class":"restart-service","op":"restart","reversible":true,"confidence":0.85,"evidence_ids":["tr-1"]}}`
)

// fakeVerdicts is a VerdictReader that returns a fixed mechanical verdict.
type fakeVerdicts struct{ v safety.Verdict }

func (f fakeVerdicts) Get(context.Context, string) (safety.Verdict, bool, error) {
	return f.v, true, nil
}

// TestInvestigateRecordsFiveMetricFamily proves the injected emitter records the FIVE agent metrics on a
// real driven loop (a tool call then a proposal): runtime, tool-call count, tool errors, approximate
// tokens, and the by-outcome run counter (the accuracy dimension) — with the expected bounded labels.
func TestInvestigateRecordsFiveMetricFamily(t *testing.T) {
	reg := observe.NewRegistry()
	deps := testDeps(investigateToolCall, proposeCitingToolResult)
	deps.Metrics = reg

	res, err := NewActivities(deps).InvestigateActivity(context.Background(),
		ingest.IncidentEnvelope{ExternalRef: "TG-1", Host: "web01", AlertRule: "NginxDown"}, "", ClusterMemberContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Proposed {
		t.Fatalf("scenario should propose: %+v", res)
	}

	out := metrics.Render(reg.Collect())
	for _, want := range []string{
		"# TYPE tg_agent_run_seconds_total counter",
		"# TYPE tg_agent_tool_calls_total counter",
		"tg_agent_tool_calls_total 1", // exactly one get-logs call
		"tg_agent_tool_errors_total 0",
		"# TYPE tg_agent_tokens_approx_total counter",
		`tg_agent_runs_total{outcome="proposed"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("investigate exposition missing %q; got:\n%s", want, out)
		}
	}
	// approximate tokens must be > 0 (the seed alone is non-empty) — assert the zero line is absent.
	if strings.Contains(out, "tg_agent_tokens_approx_total 0\n") {
		t.Fatalf("approx tokens must be > 0 for a non-empty seed; got:\n%s", out)
	}
}

// TestClassifyRecordsGovernanceDecision proves ClassifyActivity mirrors its classify:<band> decision into
// the governance-decision counter with a bounded band + withheld label.
func TestClassifyRecordsGovernanceDecision(t *testing.T) {
	reg := observe.NewRegistry()
	deps := testDeps()
	deps.Metrics = reg

	if _, err := NewActivities(deps).ClassifyActivity(context.Background(), ClassifyInput{
		ExternalRef: "TG-1", ActionID: "act-1", RiskLevel: "low",
		OpClass: "restart-service", Op: "restart", Host: "web01", Reversible: true,
	}); err != nil {
		t.Fatal(err)
	}
	out := metrics.Render(reg.Collect())
	if !strings.Contains(out, "# TYPE tg_governance_decisions_total counter") ||
		!strings.Contains(out, `tg_governance_decisions_total{band=`) ||
		!strings.Contains(out, `withheld=`) {
		t.Fatalf("classify must record a bounded governance decision; got:\n%s", out)
	}
}

// TestVerifyRecordsVerdict proves VerifyActivity records the mechanical verdict it read back for a session
// that ACTUALLY EXECUTED. Executed:true is the whole precondition — see the sibling test below.
func TestVerifyRecordsVerdict(t *testing.T) {
	reg := observe.NewRegistry()
	deps := testDeps()
	deps.Metrics = reg
	deps.Verdicts = fakeVerdicts{v: safety.VerdictMatch}

	if _, err := NewActivities(deps).VerifyActivity(context.Background(), ExecuteInput{ActionID: "act-1", Executed: true}); err != nil {
		t.Fatal(err)
	}
	out := metrics.Render(reg.Collect())
	if !strings.Contains(out, `tg_agent_verdicts_total{outcome="match"} 1`) {
		t.Fatalf("verify must record a match verdict; got:\n%s", out)
	}
}

// A SESSION THAT EXECUTED NOTHING MUST PUBLISH NO VERDICT — the invariant VerifyActivity's own doc comment
// asserted and the code did not honour.
//
// The verdict store is keyed on the CONTENT-ADDRESSED action_id and is first-wins, so two sessions producing
// the byte-identical action share one key, and repeats are the norm (measured: 113 executions collapsed into
// 28 durable outcomes). A bare Get therefore returned a PRIOR session's verdict for a session that executed
// nothing — reporting it Verified and incrementing tg_agent_verdicts_total, whose help text reads "mechanical
// POST-EXECUTION verify verdicts". The reachable case is precisely the one that matters: a host guard denying
// with exit 42 refuses the execute (REQ-1220), writes no row, and would inherit the earlier session's `match`.
func TestVerifyPublishesNothingWhenThisSessionDidNotExecute(t *testing.T) {
	reg := observe.NewRegistry()
	deps := testDeps()
	deps.Metrics = reg
	// A verdict for this action_id EXISTS — written by an earlier session. That is the trap.
	deps.Verdicts = fakeVerdicts{v: safety.VerdictMatch}

	res, err := NewActivities(deps).VerifyActivity(context.Background(), ExecuteInput{ActionID: "act-1", Executed: false})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verified {
		t.Errorf("nothing executed in this session, so nothing was verified in it; got %+v", res)
	}
	if res.Verdict != "" {
		t.Errorf("a prior session's verdict must not be republished as this session's; got %q", res.Verdict)
	}
	if out := metrics.Render(reg.Collect()); strings.Contains(out, `tg_agent_verdicts_total{outcome="match"} 1`) {
		t.Errorf("the post-execution verdict counter must not count a session with no execution; got:\n%s", out)
	}
}

// TestObserveClearedActivity proves the ConfirmedClear producer is an ORCHESTRATOR-observed HOST-QUIET check
// (INV-11): it reports cleared only when the incident host carries NO active alert, FAILS CLOSED on every
// unobservable path (nil reader, a reader that could not fetch, a blank signature), catches a worsened host,
// and never trusts a self-report.
func TestObserveClearedActivity(t *testing.T) {
	okObs := func(alerts ...verify.ObservedAlert) func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		return func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return alerts, true }
	}
	cases := []struct {
		name    string
		deps    Deps
		in      ClearInput
		cleared bool
	}{
		{"incident host quiet → cleared", Deps{ClearObserve: okObs()},
			ClearInput{Host: "web01", AlertRule: "Service-down", Site: "nl"}, true},
		{"only a DIFFERENT host alerting → cleared", Deps{ClearObserve: okObs(
			verify.ObservedAlert{Host: "db02", Rule: "HighLatency", Site: "nl"})},
			ClearInput{Host: "web01", AlertRule: "Service-down", Site: "nl"}, true},
		{"same rule still firing on the host → NOT cleared", Deps{ClearObserve: okObs(
			verify.ObservedAlert{Host: "web01", Rule: "Service-down", Site: "nl"})},
			ClearInput{Host: "WEB01", AlertRule: "service-down", Site: "nl"}, false}, // case-insensitive host
		{"host WORSENED — different rule now firing → NOT cleared", Deps{ClearObserve: okObs(
			verify.ObservedAlert{Host: "web01", Rule: "Device-unreachable", Site: "nl"})},
			ClearInput{Host: "web01", AlertRule: "Service-down", Site: "nl"}, false},
		{"reader could NOT fetch (ok=false) → fail-closed", Deps{ClearObserve: func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
			return nil, false
		}}, ClearInput{Host: "web01", AlertRule: "Service-down", Site: "nl"}, false},
		{"no reader → fail-closed", Deps{ClearObserve: nil},
			ClearInput{Host: "web01", AlertRule: "Service-down"}, false},
		{"blank signature → fail-closed", Deps{ClearObserve: okObs()},
			ClearInput{Host: "", AlertRule: ""}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NewActivities(c.deps).ObserveClearedActivity(context.Background(), c.in)
			if err != nil {
				t.Fatal(err)
			}
			if got.Cleared != c.cleared {
				t.Fatalf("Cleared = %v, want %v", got.Cleared, c.cleared)
			}
		})
	}
}

// TestRecoveredSinceActivity proves the clear-confirm BELT (TG-124 Plan B) contract: it returns TG's OWN
// captured recovery result, and it is FAIL-CLOSED + RETRY-FREE on every non-recovery path — a nil seam, a
// blank host, and a read error all return (false, nil), never a non-nil error (so a DB blip does not retry or
// fail the activity — it is simply "not recovered this tick").
func TestRecoveredSinceActivity(t *testing.T) {
	since := time.Unix(1_700_000_000, 0).UTC()
	yes := func(context.Context, string, string, time.Time) (bool, error) { return true, nil }
	no := func(context.Context, string, string, time.Time) (bool, error) { return false, nil }
	boom := func(context.Context, string, string, time.Time) (bool, error) { return false, errors.New("db blip") }
	cases := []struct {
		name string
		deps Deps
		in   RecoveredSinceInput
		want bool
	}{
		{"captured recovery → true", Deps{RecoveredSince: yes}, RecoveredSinceInput{Host: "web01", AlertRule: "Devices-up/down", Since: since}, true},
		{"no captured recovery → false", Deps{RecoveredSince: no}, RecoveredSinceInput{Host: "web01", AlertRule: "Devices-up/down", Since: since}, false},
		{"read error → fail-closed false (retry-free)", Deps{RecoveredSince: boom}, RecoveredSinceInput{Host: "web01", AlertRule: "Devices-up/down", Since: since}, false},
		{"nil seam → false (belt inert)", Deps{RecoveredSince: nil}, RecoveredSinceInput{Host: "web01", AlertRule: "Devices-up/down", Since: since}, false},
		{"blank host → false", Deps{RecoveredSince: yes}, RecoveredSinceInput{Host: "", AlertRule: "Devices-up/down", Since: since}, false},
		// An unscoped belt read would confirm on an UNRELATED rule's recovery — refuse to ask at all.
		{"blank rule → false (fail closed, never unscoped)", Deps{RecoveredSince: yes}, RecoveredSinceInput{Host: "web01", Since: since}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NewActivities(c.deps).RecoveredSinceActivity(context.Background(), c.in)
			if err != nil {
				t.Fatalf("RecoveredSinceActivity must never return a non-nil error (retry-free): %v", err)
			}
			if got != c.want {
				t.Fatalf("recovered = %v, want %v", got, c.want)
			}
		})
	}
}

// TestActivitiesNilEmitterNoPanic proves the additive emitter is a silent no-op when unwired (the oracle /
// no-DB path): the activities run unchanged with a nil Deps.Metrics and never panic.
func TestActivitiesNilEmitterNoPanic(t *testing.T) {
	deps := testDeps(investigateToolCall, proposeCitingToolResult) // testDeps leaves Metrics nil
	if deps.Metrics != nil {
		t.Fatal("precondition: testDeps must leave Metrics nil")
	}
	acts := NewActivities(deps)
	if _, err := acts.InvestigateActivity(context.Background(),
		ingest.IncidentEnvelope{ExternalRef: "TG-1", Host: "web01", AlertRule: "NginxDown"}, "", ClusterMemberContext{}); err != nil {
		t.Fatalf("investigate must not fail with a nil emitter: %v", err)
	}
	if _, err := acts.ClassifyActivity(context.Background(), ClassifyInput{
		ExternalRef: "TG-1", ActionID: "act-1", RiskLevel: "low", OpClass: "restart-service", Op: "restart", Host: "web01", Reversible: true,
	}); err != nil {
		t.Fatalf("classify must not fail with a nil emitter: %v", err)
	}
}
