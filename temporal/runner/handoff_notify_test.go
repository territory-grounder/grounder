package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/ingest"
)

// TG-386: a session that concludes "I know what is wrong, no safe action exists, a human is needed" reached a
// Postgres row and stopped — the notify call sat in the propose path, so the one investigation of 156 that
// named dc1pve03 as the cascade root cause was never pushed to a human, eleven hours before one found it.
// These oracles pin the filter, the wiring, and the inert-until-armed gate.

func TestHandoffEligible(t *testing.T) {
	deep := string(execclass.DeepInvestigation)
	cases := []struct {
		name                       string
		conclusion, class, outcome string
		want                       bool
	}{
		{"deep-investigation with a conclusion pages", "needs a human on pve03", deep, "no-proposal:stop", true},
		{"handoff-limit escalation with a conclusion pages", "ran out of options", "STANDARD_AGENT", "escalated:handoff-limit", true},
		{"empty conclusion never pages", "", deep, "escalated:handoff-limit", false},
		{"whitespace conclusion never pages", "   ", deep, "no-proposal:stop", false},
		{"a shallow ordinary stop does not page", "self-resolved", "FAST_AGENT", "no-proposal:stop", false},
	}
	for _, c := range cases {
		if got := handoffEligible(c.conclusion, c.class, c.outcome); got != c.want {
			t.Errorf("%s: handoffEligible(%q,%q,%q)=%v, want %v", c.name, c.conclusion, c.class, c.outcome, got, c.want)
		}
	}
}

// mockInvestigate overrides the real loop so the terminal state is controlled exactly.
func mockInvestigate(env *testsuite.TestWorkflowEnvironment, a *Activities, r InvestigateResult) {
	env.OnActivity(a.InvestigateActivity, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(r, nil)
}

// TestHandoffPageIsScheduledForASubstantiveTerminal — an escalate-with-conclusion terminal schedules a
// NotifyActivity whose body carries the conclusion. Killing mutation: move the notify block below `return res,
// nil` in the !inv.Proposed branch (compiles, unreachable) → the page is never scheduled → RED.
func TestHandoffPageIsScheduledForASubstantiveTerminal(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(`unused — investigate is mocked`)
	deps.HandoffNotify = true // armed, so the activity would deliver — the schedule is what this asserts
	a := NewActivities(deps)
	registerAll(env, a)

	const conclusion = "no guest-level action is safe; a human is needed on dc1pve03"
	// VACUITY GUARD: the fixture must carry a real conclusion, or the test could pass by paging on nothing.
	if strings.TrimSpace(conclusion) == "" {
		t.Fatal("fixture conclusion is empty — the assertion below would be vacuous")
	}
	mockInvestigate(env, a, InvestigateResult{
		Proposed:   false,
		Outcome:    agent.OutcomeEscalate.String(), // ⇒ res.Outcome = "escalated:handoff-limit"
		Conclusion: conclusion,
		Reason:     "handoff limit reached",
	})

	var body string
	var handoff, scheduled bool
	env.OnActivity(a.NotifyActivity, mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		in := args.Get(1).(NotifyInput)
		if in.Handoff {
			body, handoff, scheduled = in.Body, in.Handoff, true
		}
	}).Return(NotifyResult{Delivered: true}, nil)

	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{ExternalRef: "TG-handoff-1",
		SourceID: "prometheus-dc1", AlertRule: "NFSStaleFhExporterDown", Host: "dc1cl01file02",
		Severity: ingest.SeverityWarning, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	if !scheduled {
		t.Fatal("a substantive proposal-less terminal did NOT schedule a handoff page — the conclusion reaches " +
			"a Postgres row and stops, exactly the TG-386 defect")
	}
	if !handoff {
		t.Error("the scheduled notice was not marked Handoff — it would bypass the arming gate")
	}
	if !strings.Contains(body, conclusion) {
		t.Errorf("the handoff page body does not carry the conclusion a human needs to act on: %q", body)
	}
}

// TestNoHandoffPageForAnEmptyConclusion — the filter must hold: a no-proposal stop with NO conclusion pages
// nobody. Killing mutation: make handoffEligible unconditional (drop the empty-conclusion guard) → RED.
func TestNoHandoffPageForAnEmptyConclusion(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(`unused`)
	deps.HandoffNotify = true
	a := NewActivities(deps)
	registerAll(env, a)

	mockInvestigate(env, a, InvestigateResult{
		Proposed: false, Outcome: agent.OutcomeEscalate.String(), Conclusion: "", Reason: "model call failed",
	})

	var handoffScheduled bool
	env.OnActivity(a.NotifyActivity, mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		if args.Get(1).(NotifyInput).Handoff {
			handoffScheduled = true
		}
	}).Return(NotifyResult{Delivered: true}, nil)

	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{ExternalRef: "TG-handoff-empty",
		SourceID: "prometheus-dc1", AlertRule: "NginxDown", Host: "web01",
		Severity: ingest.SeverityWarning, Site: "dc1"})
	if handoffScheduled {
		t.Error("an empty-conclusion terminal scheduled a handoff page — paging on every no-action stop fires " +
			"on 35.8% of sessions and re-introduces the chattiness the filter exists to prevent")
	}
}

// TestHandoffNotifyIsInertUntilArmed — the activity-level gate: a handoff notice is NOT delivered until the
// operator arms TG_HANDOFF_NOTIFY_ENABLED (Deps.HandoffNotify), while a governance notice (Handoff=false) is
// unaffected. This is what keeps the wiring safe to ship: the code lands, the pages do not, until armed.
func TestHandoffNotifyIsInertUntilArmed(t *testing.T) {
	// A fake notifier that counts deliveries.
	deps := testDeps(`x`)
	var count int
	deps.Notify = func(context.Context, notifier.Notice) error { count++; return nil }

	// disarmed: a handoff notice is dropped at the activity.
	deps.HandoffNotify = false
	a := NewActivities(deps)
	res, err := a.NotifyActivity(context.Background(), NotifyInput{DecisionID: "r1", Handoff: true, Body: "b"})
	if err != nil || res.Delivered {
		t.Fatalf("a handoff page must NOT deliver while disarmed, got delivered=%v err=%v", res.Delivered, err)
	}
	if count != 0 {
		t.Fatalf("the notifier was called %d times while disarmed — nothing should reach the channel", count)
	}
	// a governance notice (not a handoff) is unaffected by the handoff gate.
	if r, _ := a.NotifyActivity(context.Background(), NotifyInput{DecisionID: "r2", Handoff: false, Body: "gov"}); !r.Delivered {
		t.Error("a governance notice must still deliver — the handoff gate must not touch the proposed lane")
	}

	// armed: the handoff page delivers.
	deps.HandoffNotify = true
	a2 := NewActivities(deps)
	if r, _ := a2.NotifyActivity(context.Background(), NotifyInput{DecisionID: "r3", Handoff: true, Body: "b"}); !r.Delivered {
		t.Error("an ARMED handoff page must deliver")
	}
}
