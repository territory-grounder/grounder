package runner

// spec/030 T-030-4 — the plan poll at the propose terminal (TG-58, REQ-3001/3002). The claims, each an
// outcome oracle over the REAL RunnerWorkflow (the vote_admission_test harness — mutation ON, wired
// chain — so "the actuator never ran" is an observation, not an assumption):
//
//   1. A declared recipe matching the proposed op-class turns the poll into the PLAN offer: the pending
//      projection binds plan:<plan_id> and renders every step WITH its compensation; ONE well-bound
//      approval starts the saga child carrying the composed steps and the voter — and the single action
//      NEVER executes (the plan replaced it in the offer).
//   2. REQ-3002 binding: with a plan offered, a vote naming the sealed SINGLE action is MISBOUND —
//      counted, never obeyed — and the elapsed window denies with the child never started.
//   3. A well-bound DENY stands the plan down: no child, no execution.
//   4. With no recipe declared (the shipped catalog), the single lane is unchanged: the poll binds the
//      sealed action id and an approval executes it exactly once.
//
// KILLING MUTATIONS (executed 2026-08-22, red→green evidence):
//   - planBindID substitution dropped in workflow.go (bind gate.ActionID even when a plan is offered) →
//     oracle 1 red (the plan-bound vote is misbound, the poll times out) AND oracle 2 red (the
//     single-action vote APPROVES the plan). Restored, green.
//   - the plancomp.Matched child branch dropped → oracle 1 red (no child ran; the single action
//     executed under a plan approval). Restored, green.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/plan"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// tpPollRecipe is a valid recipe over REGISTERED op-classes whose trigger matches the harness's
// proposed action (restart-service, params {"unit":"nginx"} on web01 — proposeCanaryReversible).
func tpPollRecipe() plan.PlanRecipe {
	return plan.PlanRecipe{
		Name:           "restart-then-settle",
		TriggerOpClass: "restart-service",
		Steps: []plan.PlanStep{
			{OpClass: "reload-service", ParamsFrom: map[string]string{"unit": "unit"}},
			{OpClass: "restart-service", ParamsFrom: map[string]string{"unit": "unit"}},
		},
	}
}

// tpPollOutcome is everything the oracles assert on from one run.
type tpPollOutcome struct {
	res      RunnerResult
	execs    int
	childRan bool
	childIn  TransactionPlanInput
	pending  PendingDecisionInput
}

// tpPollRun drives the real RunnerWorkflow to its POLL_PAUSE poll with a recipe catalog injected (or
// not), delivers one vote whose binding the caller chooses, and reports the terminal, the actuator's
// run count, and what the child/projection saw. voteFor may be nil for a no-vote (window elapse) run.
func tpPollRun(t *testing.T, inject bool, voteFor func(planID, actionID string) (string, bool)) tpPollOutcome {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	deps := testDeps(
		`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,
		proposeCanaryReversible,
	)
	gate := safety.NewActuatingChokepoint() // mutation ON (test-only): "nothing executed" is an observation
	act := &recordingActuator{}
	sink := &fakeManifestSink{}
	deps.Mutation = gate
	deps.Interceptor = withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger()))
	deps.Manifests = sink
	deps.ManifestSink = sink
	deps.PostStateObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }
	deps.ClearObserve = faultedUntilHealed("web01", "NginxDown", act)
	deps.CanaryPinned = func(string, string) (bool, string) { return true, "canary: staged first mutation" }
	if inject {
		deps.PlanRecipes = func() ([]plan.PlanRecipe, error) { return []plan.PlanRecipe{tpPollRecipe()}, nil }
	}
	acts := NewActivities(deps)
	registerAll(env, acts)
	env.RegisterActivity(acts.BackfillManifestActivity)
	env.RegisterActivity(acts.ObserveClearedActivity)
	env.RegisterActivity(acts.RecoveredSinceActivity)
	env.RegisterActivity(acts.ReconcileActivity)
	env.RegisterActivity(acts.ResolvePendingActivity)
	env.RegisterWorkflow(TransactionPlanWorkflow)

	out := tpPollOutcome{}
	env.OnActivity(acts.RecordPendingActivity, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { out.pending = args.Get(1).(PendingDecisionInput) }).
		Return(RecordPendingResult{Recorded: true}, nil)
	env.OnWorkflow(TransactionPlanWorkflow, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			out.childIn = args.Get(1).(TransactionPlanInput)
			out.childRan = true
		}).
		Return(TransactionPlanResult{Terminal: "committed", StepsExecuted: 2}, nil)

	planID, err := plan.PlanID(tpPollRecipe(), map[string]string{"unit": "nginx"})
	if err != nil {
		t.Fatalf("derive the expected plan id: %v", err)
	}
	actionID, err := proposedActionID()
	if err != nil {
		t.Fatalf("derive the sealed single-action id: %v", err)
	}
	if voteFor != nil {
		bind, approve := voteFor(planID, actionID)
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(VoteSignalName, VoteSignal{Approve: approve, Voter: "operator", ActionID: bind})
		}, time.Minute)
	}
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-plan-poll", SourceID: "prometheus-dc1", AlertRule: "NginxDown",
		Host: "web01", Severity: ingest.SeverityWarning, Site: "dc1",
	})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	if err := env.GetWorkflowResult(&out.res); err != nil {
		t.Fatalf("read the workflow result: %v", err)
	}
	if out.res.Band != safety.BandPollPause.String() {
		t.Fatalf("the fixture must reach a POLL_PAUSE poll (else nothing is being voted on), band=%q: %+v", out.res.Band, out.res)
	}
	out.execs = act.execs
	return out
}

// Oracle 1 — the plan offer, approved once, runs the saga and ONLY the saga.
func TestPlanPollOneApprovalBoundToThePlanRunsTheSaga(t *testing.T) {
	out := tpPollRun(t, true, func(planID, _ string) (string, bool) { return "plan:" + planID, true })
	if !out.childRan {
		t.Fatal("the approved plan must start TransactionPlanWorkflow — no child ran")
	}
	if out.execs != 0 {
		t.Fatalf("the SINGLE action must never execute when the plan replaced it in the offer — actuator ran %d time(s)", out.execs)
	}
	wantID, _ := plan.PlanID(tpPollRecipe(), map[string]string{"unit": "nginx"})
	if out.childIn.PlanID != wantID || out.childIn.Recipe != "restart-then-settle" {
		t.Fatalf("the child must carry the composed plan (id %s), got %+v", wantID, out.childIn)
	}
	if out.childIn.ApprovedVoter != "operator" {
		t.Fatalf("the child must carry the ONE taken approval's voter, got %q", out.childIn.ApprovedVoter)
	}
	if len(out.childIn.Steps) != 2 || out.childIn.Steps[0].OpClass != "reload-service" ||
		out.childIn.Steps[0].Params["unit"] != "nginx" || out.childIn.Steps[1].Params["unit"] != "nginx" {
		t.Fatalf("the steps must be the recipe's, params rendered from the trigger's screened params: %+v", out.childIn.Steps)
	}
	if out.pending.ActionID != "plan:"+wantID {
		t.Fatalf("REQ-3002: the poll projection must bind the plan token, got %q", out.pending.ActionID)
	}
	if len(out.pending.Approaches) != 2 ||
		!strings.Contains(out.pending.Approaches[0], "undo:") || !strings.Contains(out.pending.Approaches[1], "undo:") {
		t.Fatalf("REQ-3002: the vote surface must render every step AND its compensation, got %q", out.pending.Approaches)
	}
	if !strings.HasPrefix(out.res.Outcome, "plan:committed") {
		t.Fatalf("the session terminal must carry the plan terminal, got %q", out.res.Outcome)
	}
}

// Oracle 2 — with a plan offered, a vote naming the sealed SINGLE action is misbound: counted, never
// obeyed; the window elapses to a deny and the child never starts.
func TestPlanPollVoteNamingTheSingleActionIsMisbound(t *testing.T) {
	out := tpPollRun(t, true, func(_, actionID string) (string, bool) { return actionID, true })
	if out.res.Vote != "timeout" {
		t.Fatalf("a misbound vote must not decide the plan — want the window to elapse (timeout), got vote=%q outcome=%q", out.res.Vote, out.res.Outcome)
	}
	if out.childRan || out.execs != 0 {
		t.Fatalf("NOTHING may run on a misbound vote: childRan=%v execs=%d", out.childRan, out.execs)
	}
}

// Oracle 3 — a well-bound deny stands the plan down without starting anything.
func TestPlanPollWellBoundDenyStandsThePlanDown(t *testing.T) {
	out := tpPollRun(t, true, func(planID, _ string) (string, bool) { return "plan:" + planID, false })
	if out.res.Vote != "denied" {
		t.Fatalf("want the plan denied, got vote=%q", out.res.Vote)
	}
	if out.childRan || out.execs != 0 {
		t.Fatalf("a denied plan must run NOTHING: childRan=%v execs=%d", out.childRan, out.execs)
	}
}

// Oracle 4 — the shipped catalog (empty) leaves the single lane byte-identical: the poll binds the
// sealed action id and one approval executes it exactly once, no child anywhere.
func TestPlanPollNoRecipeLeavesTheSingleLaneUnchanged(t *testing.T) {
	out := tpPollRun(t, false, func(_, actionID string) (string, bool) { return actionID, true })
	if out.pending.ActionID == "" || strings.HasPrefix(out.pending.ActionID, "plan:") {
		t.Fatalf("with no recipe the poll must bind the sealed single action, got %q", out.pending.ActionID)
	}
	if out.res.Vote != "approved" || out.execs != 1 {
		t.Fatalf("the single lane must be unchanged (approve executes once), got vote=%q execs=%d", out.res.Vote, out.execs)
	}
	if out.childRan {
		t.Fatal("no recipe, no child — the plan lane must be structurally silent")
	}
}

// Every compose failure direction is Matched=false with a nil error — the single action as today,
// never a half-composed plan and never a failed session (the lookup is an OFFER, not a gate).
func TestComposePlanFailureDirectionsAreSingleActionAsToday(t *testing.T) {
	ctx := context.Background()
	trigger := ComposePlanInput{OpClass: "restart-service", Target: "web01", Params: map[string]string{"unit": "nginx"}, ExternalRef: "TG-compose"}
	cases := []struct {
		name    string
		catalog func() ([]plan.PlanRecipe, error)
		in      ComposePlanInput
	}{
		{"no recipe matches the op-class", func() ([]plan.PlanRecipe, error) { return []plan.PlanRecipe{tpPollRecipe()}, nil },
			ComposePlanInput{OpClass: "start-guest", Target: "g1", Params: map[string]string{"guest": "101"}}},
		{"catalog invalid", func() ([]plan.PlanRecipe, error) { return nil, errors.New("bad catalog") }, trigger},
		{"mapped trigger param absent", func() ([]plan.PlanRecipe, error) {
			r := tpPollRecipe()
			r.Steps[0].ParamsFrom = map[string]string{"unit": "no-such-trigger-param"}
			return []plan.PlanRecipe{r}, nil
		}, trigger},
		{"unregistered step class", func() ([]plan.PlanRecipe, error) {
			r := tpPollRecipe()
			r.Steps[0].OpClass = "no-such-class"
			return []plan.PlanRecipe{r}, nil
		}, trigger},
	}
	for _, c := range cases {
		a := NewActivities(Deps{PlanRecipes: c.catalog})
		out, err := a.ComposePlanActivity(ctx, c.in)
		if err != nil || out.Matched {
			t.Errorf("%s: want Matched=false with nil error (single action as today), got matched=%v err=%v", c.name, out.Matched, err)
		}
	}
	// VACUITY GUARD: the same trigger against the intact catalog DOES compose — else every case above
	// passes because compose never works at all.
	a := NewActivities(Deps{PlanRecipes: func() ([]plan.PlanRecipe, error) { return []plan.PlanRecipe{tpPollRecipe()}, nil }})
	out, err := a.ComposePlanActivity(ctx, trigger)
	if err != nil || !out.Matched || len(out.Steps) != 2 {
		t.Fatalf("the intact catalog must compose (matched=%v steps=%d err=%v) — without this the refusal cases are vacuous", out.Matched, len(out.Steps), err)
	}
}
