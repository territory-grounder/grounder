package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/core/safety"
)

// regimeExecManifest seals a reversible restart-service action for the regime-routing tests.
func regimeExecManifest(t *testing.T) *manifest.ActionManifest {
	t.Helper()
	m, err := manifest.New(manifest.Action{Target: "web01", OpClass: "restart-service", Op: "restart", Params: map[string]string{"unit": "nginx"}, Reversible: true}, safety.BandAuto, "plan#r", "pred#r")
	if err != nil {
		t.Fatalf("seal manifest: %v", err)
	}
	return m
}

// TestExecuteActivityRoutesThroughRegimeEngineAndPreservesChokepoint proves the spec/017 dispatch routing
// (REQ-1700/1702): when the RegimeEngine + LaneEffect are wired and there is NO direct Interceptor in Deps,
// the execute activity dispatches through SelectLane → LaneEffect → a per-lane spec/013 interceptor. Under
// mutation OFF, with a genuinely MUTATING (recording) native-ssh lane leaf, it STILL refuses at the mode
// chokepoint and NEVER reaches the effect leaf (execs==0) — the routed path is not a weaker path than the
// direct one. The refusal is recorded on the ledger, proving the dispatch went THROUGH a real interceptor
// (the composition seam), not around it.
func TestExecuteActivityRoutesThroughRegimeEngineAndPreservesChokepoint(t *testing.T) {
	ctx := context.Background()
	gate := safety.NewReadOnlyChokepoint() // mutation OFF
	ledger := audit.NewLedger()
	act := &recordingActuator{} // mutating leaf — must NEVER be reached at Shadow

	// The single-source builder: a per-lane interceptor carrying the required chain (mode chokepoint + ledger).
	builder := func(leaf actuation.Actuator) *actuate.Interceptor { return actuate.NewInterceptor(gate, leaf, ledger) }
	laneEffect := regime.NewLaneEffect(builder)
	sshLane := regime.NewNativeSSHLane(act)
	engine := regime.NewEngine(nil, []regime.Lane{sshLane}, regime.WithDefaultLane(sshLane))

	m := regimeExecManifest(t)
	sink := &fakeManifestSink{}
	_ = sink.Seal(ctx, m)

	// No direct Interceptor in Deps — the routing MUST use the engine (or the execute is a no-op and this fails).
	acts := NewActivities(Deps{RegimeEngine: engine, LaneEffect: laneEffect, Manifests: sink, Mutation: gate})
	res, err := acts.ExecuteActivity(ctx, ExecuteInput{ActionID: m.ActionID, PlanHash: "plan#r", Band: safety.BandAuto, TargetHost: "web01"})
	if err != nil {
		t.Fatalf("routed execute errored (should be a recorded refusal, not an error): %v", err)
	}
	if res.Executed {
		t.Fatal("routed execute reported Executed=true while mutation is OFF — the estate must never be mutated")
	}
	if act.execs != 0 {
		t.Fatalf("routed execute reached the MUTATING effect leaf %d time(s) at Shadow — the routed path bypassed the chokepoint", act.execs)
	}
	found := false
	for _, e := range ledger.Entries() {
		if strings.HasPrefix(e.Decision, "actuate:") {
			found = true
		}
	}
	if !found {
		t.Fatal("routed execute did not go through a real interceptor — no actuate: decision on the ledger (dark seam)")
	}
}

// TestExecuteActivityFailsClosedWhenNoLaneResolves proves the routing fails CLOSED (REQ-1701): an engine with
// no matching rule AND no default lane refuses the target, so the execute activity records a NON-executed
// refusal that names the missing lane and NEVER touches an effect leaf — it does not fall through to any
// direct actuator.
func TestExecuteActivityFailsClosedWhenNoLaneResolves(t *testing.T) {
	ctx := context.Background()
	gate := safety.NewReadOnlyChokepoint()
	ledger := audit.NewLedger()
	act := &recordingActuator{}
	builder := func(leaf actuation.Actuator) *actuate.Interceptor { return actuate.NewInterceptor(gate, leaf, ledger) }
	laneEffect := regime.NewLaneEffect(builder)
	// A native-ssh lane exists in the registry, but there is NO default lane and NO rule → an unmatched target
	// resolves to no lane (ErrNoRegime), so SelectLane refuses (never a guessed lane).
	engine := regime.NewEngine(nil, []regime.Lane{regime.NewNativeSSHLane(act)})

	m := regimeExecManifest(t)
	sink := &fakeManifestSink{}
	_ = sink.Seal(ctx, m)

	acts := NewActivities(Deps{RegimeEngine: engine, LaneEffect: laneEffect, Manifests: sink, Mutation: gate})
	res, err := acts.ExecuteActivity(ctx, ExecuteInput{ActionID: m.ActionID, PlanHash: "plan#r", Band: safety.BandAuto, TargetHost: "web01"})
	if err != nil {
		t.Fatalf("no-lane execute errored (should be a recorded refusal): %v", err)
	}
	if res.Executed {
		t.Fatal("no-lane execute reported Executed=true — a target with no effect lane must never actuate")
	}
	if !strings.Contains(res.Note, "no effect lane") {
		t.Fatalf("no-lane refusal did not name the missing lane; note=%q", res.Note)
	}
	if act.execs != 0 {
		t.Fatalf("no-lane execute reached the effect leaf %d time(s) — it fell through instead of failing closed", act.execs)
	}
}
