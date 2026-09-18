package runner

// TG-122 slice 0 — the deferred-verify PRODUCER in the execute dispatch (REQ-1709/1712). Pins:
// Reserve-BEFORE-launch ordering, BindHandle with the captured handle after an executed launch, the governed
// duplicate refusal (no re-launch, leaf untouched), the byte-identical structural refusal when the producer
// seam is nil, and the withheld inline verdict (PostStateObserve is never consulted for an async launch).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// asyncHandleActuator answers a launch with a job handle on stdout (the awxjob shape).
type asyncHandleActuator struct {
	execs  int
	stdout string
}

func (a *asyncHandleActuator) Capability() string { return "awx-job" }
func (a *asyncHandleActuator) ReadOnly() bool     { return false }
func (a *asyncHandleActuator) Exec(context.Context, []string, []byte) (actuation.Result, error) {
	a.execs++
	return actuation.Result{Stdout: []byte(a.stdout)}, nil
}

// fakeAsyncLaunch records the producer calls in order.
type fakeAsyncLaunch struct {
	reserveErr error
	intents    []regime.LaunchIntent
	binds      [][2]string
	order      []string
}

func (f *fakeAsyncLaunch) Reserve(_ context.Context, intent regime.LaunchIntent) error {
	f.order = append(f.order, "reserve")
	if f.reserveErr != nil {
		return f.reserveErr
	}
	f.intents = append(f.intents, intent)
	return nil
}
func (f *fakeAsyncLaunch) BindHandle(_ context.Context, actionID, jobID string) error {
	f.order = append(f.order, "bind")
	f.binds = append(f.binds, [2]string{actionID, jobID})
	return nil
}

// permissiveRunnerDecider mirrors core/regime's permissiveTestDecider: since REQ-1207c an ACTUATING
// interceptor with no policy authorizer refuses, and this file's subject is the producer, not policy.
type permissiveRunnerDecider struct{}

func (permissiveRunnerDecider) Decide(_ context.Context, in policy.EvalInput) (policy.PolicyDecision, error) {
	return policy.NewPolicyDecision(policy.VerdictAuto, "test-permissive", in.Band, nil, in.Mode, "test", policy.DecisionAudit{}), nil
}

// asyncProducerRig wires an engine whose ONLY route for web01 is a fully-actuator-injected awx-job lane,
// behind an ACTUATING chain, plus a PostStateObserve recorder (which must stay unconsulted on this path).
func asyncProducerRig(t *testing.T, launch *fakeAsyncLaunch) (*Activities, *asyncHandleActuator, *manifest.ActionManifest, *int) {
	t.Helper()
	leaf := &asyncHandleActuator{stdout: "job-7\n"}
	lane := regime.NewAWXJobLane(regime.WithAWXActuator(leaf))
	engine := regime.NewEngine(
		[]regime.Rule{{ID: "r-awx", Selector: credential.Selector{Kind: credential.KindHost, Pattern: "web01"}, Regime: regime.RegimeAWXJob}},
		[]regime.Lane{lane},
	)
	gate := safety.NewActuatingChokepoint()
	builder := func(l actuation.Actuator) *actuate.Interceptor {
		return actuate.NewInterceptor(gate, l, audit.NewLedger()).
			WithPolicyDecider(permissiveRunnerDecider{}, func() policy.Mode { return policy.ModeFullAuto })
	}
	m := regimeExecManifest(t)
	sink := &fakeManifestSink{}
	_ = sink.Seal(context.Background(), m)
	observed := 0
	deps := Deps{
		RegimeEngine: engine,
		LaneEffect:   regime.NewLaneEffect(builder),
		Manifests:    sink,
		Mutation:     gate,
		PostStateObserve: func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
			observed++
			return nil, true
		},
		// Gate 4i necessity: the target still carries its active alert, so the launch is not refuted.
		ClearObserve: func(_ context.Context, host, _ string) ([]verify.ObservedAlert, bool) {
			return []verify.ObservedAlert{{Host: host}}, true
		},
		// The HOST arm of the pre-action baseline (req.PreAnomalous). The async branch's deferred observer
		// answers (nil,false), so the PAIR arm is unavailable by design — the baseline gate stands on this
		// arm, exactly as on a production deployment (OpenIncidents is wired wherever the pool is).
		OpenIncidents: func(context.Context, time.Time) (map[string]bool, bool) {
			return map[string]bool{"web01": true}, true
		},
	}
	if launch != nil {
		deps.AsyncLaunch = launch
	}
	acts := NewActivities(deps)
	return acts, leaf, m, &observed
}

func TestExecuteActivityReservesThenLaunchesThenBinds(t *testing.T) {
	launch := &fakeAsyncLaunch{}
	acts, leaf, m, observed := asyncProducerRig(t, launch)
	res, err := acts.ExecuteActivity(context.Background(), ExecuteInput{
		ActionID: m.ActionID, Band: safety.BandAuto, TargetHost: "web01",
		EvidenceIDs: []string{"tr-1"},
		ToolResults: []agent.ToolResult{{ID: "tr-1", Target: "web01", Output: "web01 nginx is failed", Success: true}},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !res.Executed {
		t.Fatalf("the reserved async launch must report executed (launched), got %+v", res)
	}
	if leaf.execs != 1 {
		t.Fatalf("leaf execs = %d, want 1", leaf.execs)
	}
	// Reserve BEFORE the launch, bind AFTER — the at-most-one-launch guard is worthless post-hoc.
	if len(launch.order) != 2 || launch.order[0] != "reserve" || launch.order[1] != "bind" {
		t.Fatalf("producer call order = %v, want [reserve bind] around the launch", launch.order)
	}
	if len(launch.intents) != 1 || launch.intents[0].ActionID != m.ActionID ||
		launch.intents[0].OpClass != "restart-service" || launch.intents[0].Lane != regime.RegimeAWXJob {
		t.Fatalf("Reserve intent = %+v, want this action/op-class on the awx-job lane", launch.intents)
	}
	if len(launch.binds) != 1 || launch.binds[0] != [2]string{m.ActionID, "job-7"} {
		t.Fatalf("BindHandle = %v, want the trimmed captured handle bound to the action", launch.binds)
	}
	// The inline verdict is WITHHELD: the post-state observer must never be consulted for a launch whose
	// effect has not landed (TG-182 / INV-10 — the deferred channel is the sole verdict author).
	// KILLING MUTATION: drop the deferred-observer substitution (areq.Observe = (nil,false)) → the chain
	// consults PostStateObserve and adjudicates the untouched estate, and this reddens.
	if *observed != 0 {
		t.Fatalf("PostStateObserve consulted %d time(s) on an async launch — inline adjudication of an unlanded effect", *observed)
	}
	if res.Verdict != "" {
		t.Fatalf("no inline verdict may be minted for an async launch, got %q", res.Verdict)
	}
	if !strings.Contains(res.Note, "pending deferred verification") {
		t.Errorf("the result must SAY the verdict is deferred, got %q", res.Note)
	}
}

func TestExecuteActivityRefusesDuplicateAsyncLaunch(t *testing.T) {
	launch := &fakeAsyncLaunch{reserveErr: regime.ErrDuplicateLaunch}
	acts, leaf, m, _ := asyncProducerRig(t, launch)
	res, err := acts.ExecuteActivity(context.Background(), ExecuteInput{
		ActionID: m.ActionID, Band: safety.BandAuto, TargetHost: "web01",
		EvidenceIDs: []string{"tr-1"},
		ToolResults: []agent.ToolResult{{ID: "tr-1", Target: "web01", Output: "web01 nginx is failed", Success: true}},
	})
	if err != nil {
		t.Fatalf("a duplicate must be a GOVERNED refusal, not an error: %v", err)
	}
	if res.Executed || leaf.execs != 0 {
		t.Fatalf("a duplicate reservation must never re-launch (REQ-1712), got %+v execs=%d", res, leaf.execs)
	}
	if !strings.Contains(res.Note, "duplicate") {
		t.Errorf("the refusal must name the duplicate, got %q", res.Note)
	}
}

// With NO producer seam wired, the pre-slice-0 posture is byte-identical: the structural synchronous-path
// refusal stands and the leaf is never reached. KILLING MUTATION: route the nil-producer case through
// ApplyReserved → the launch fires unreserved and this reddens.
func TestExecuteActivityKeepsStructuralRefusalWithoutProducer(t *testing.T) {
	acts, leaf, m, _ := asyncProducerRig(t, nil)
	res, err := acts.ExecuteActivity(context.Background(), ExecuteInput{
		ActionID: m.ActionID, Band: safety.BandAuto, TargetHost: "web01",
		EvidenceIDs: []string{"tr-1"},
		ToolResults: []agent.ToolResult{{ID: "tr-1", Target: "web01", Output: "web01 nginx is failed", Success: true}},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Executed || leaf.execs != 0 {
		t.Fatalf("without the producer the async lane must keep the structural refusal, got %+v execs=%d", res, leaf.execs)
	}
	if !strings.Contains(res.Note, "handle") {
		t.Errorf("the refusal must be the LaneEffect structural one, got %q", res.Note)
	}
}
