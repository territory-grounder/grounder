package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// necessity_wire_test.go — TG-166(b) at the WIRING seam. The interceptor gate is proven in core/actuate; what
// is proven here is that the runner actually SUPPLIES the probe, and supplies the right one. A gate whose seam
// nothing fills is a gate that refuses everything (the fail-closed half) while checking nothing (the useful
// half), so this is the half that decides whether the control does any work in production.
//
// The probe is deliberately the SAME reader the clear-check uses (Deps.ClearObserve, the live active-alert
// surface), asked the same host-quiet question BEFORE the mutation instead of after: if that read is trusted
// to auto-close an incident, it is trusted to say the incident is already closed.

// executeWith runs ONE ExecuteActivity against a fully-grounded sealed action, with the supplied ClearObserve
// wiring, and reports the result plus how many times the effect leaf was actually reached.
func executeWith(t *testing.T, clear func(context.Context, string, string) ([]verify.ObservedAlert, bool)) (ExecuteResult, int) {
	t.Helper()
	ctx := context.Background()
	gate := safety.NewActuatingChokepoint() // mutation ON (test-only)
	act := &recordingActuator{}
	m := unitManifest(t)
	sink := &fakeManifestSink{}
	if err := sink.Seal(ctx, m); err != nil {
		t.Fatalf("seal: %v", err)
	}
	deps := Deps{
		Interceptor: withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger())),
		Manifests:   sink,
		Mutation:    gate,
		PostStateObserve: func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
			return []verify.ObservedAlert{}, true
		},
		ClearObserve: clear,
	}
	res, err := NewActivities(deps).ExecuteActivity(ctx, ExecuteInput{
		ActionID: m.ActionID, ExternalRef: "TG-necessity", PlanHash: "plan#n", TargetHost: "web01", Site: "nl",
		Band:        safety.BandAuto,
		EvidenceIDs: []string{"tr-1"},
		ToolResults: []agent.ToolResult{{ID: "tr-1", Target: "web01", Output: "web01 nginx is failed", Success: true}},
	})
	if err != nil {
		t.Fatalf("execute must be a recorded refusal or an execution, never an error: %v", err)
	}
	return res, act.execs
}

// THE LIVE SHAPE THIS CLOSES: the incident is raised, TG investigates, the model proposes a restart — and by
// the time the sealed action reaches the effect the host has already recovered (systemd's own Restart=, an
// operator, config management). Before TG-166b the restart fired anyway, dropping live connections on a
// healthy box and crediting restart-service with a clean run for a non-event.
//
// KILLING MUTATION (executed 2026-08-04): in temporal/runner/activities.go's ExecuteActivity, delete the
// `req.StillFaulted = …` wiring block. The interceptor then refuses for a DIFFERENT reason (no re-check
// wired), so this test's reason assertion is what discriminates: it FAILS with
//
//	"the execute activity did not wire the necessity probe: got %q, want the gate's NO LONGER NECESSARY
//	 wording. An unwired seam refuses everything, which reads like a working control while checking nothing."
//
// Restored → green.
func TestExecuteActivityRefusesWhenTheFaultAlreadyCleared(t *testing.T) {
	res, execs := executeWith(t, func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		return nil, true // the host is QUIET at execute time — whatever justified the mutation is gone
	})
	if res.Executed || execs != 0 {
		t.Fatalf("a host that healed between propose and execute must NOT be mutated: %+v execs=%d", res, execs)
	}
	if !strings.Contains(res.Note, "NO LONGER NECESSARY") {
		t.Fatalf("the execute activity did not wire the necessity probe: got %q, want the gate's NO LONGER "+
			"NECESSARY wording. An unwired seam refuses everything, which reads like a working control while "+
			"checking nothing.", res.Note)
	}
}

// THE CONTROL AGAINST OVER-REFUSAL, at the wiring seam: a host that is STILL alerting must still be healed.
// Without this case the test above is satisfied by a runner that refuses every actuation.
func TestExecuteActivityStillHealsAHostThatIsStillAlerting(t *testing.T) {
	res, execs := executeWith(t, func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		return []verify.ObservedAlert{{Host: "web01", Rule: "NginxDown", Site: "nl"}}, true
	})
	if !res.Executed || execs != 1 {
		t.Fatalf("a host whose fault is STILL present must still be healed — a necessity check that refuses "+
			"everything is an outage, not a control: %+v execs=%d", res, execs)
	}
}

// A READ ERROR IS NOT A CLEAR, and it is not a licence either. ClearObserve propagates observability
// (ok=false on a fetch/token/HTTP failure, TG-182), and the runner must pass that through rather than
// flatten it — a monitoring outage must not decide, in either direction, whether the estate gets mutated.
func TestExecuteActivityRefusesWhenTheNecessityProbeCannotRead(t *testing.T) {
	res, execs := executeWith(t, func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		return nil, false // the reader could not fetch
	})
	if res.Executed || execs != 0 {
		t.Fatalf("an unreadable monitoring surface must not license a mutation: %+v execs=%d", res, execs)
	}
	if !strings.Contains(res.Note, "could not be re-observed") {
		t.Fatalf("a read error must surface as its own refusal, not as a clear: %q", res.Note)
	}
}

// NO READER AT ALL ⇒ REFUSE. A deployment with no live alert surface cannot re-check necessity, so it does not
// actuate. This is the same call gate 4c makes for a missing post-execution observer, made at the seam that
// decides whether the deployment has the control.
func TestExecuteActivityRefusesWithNoClearReaderWired(t *testing.T) {
	res, execs := executeWith(t, nil)
	if res.Executed || execs != 0 {
		t.Fatalf("a deployment with no alert reader cannot re-check necessity and must not actuate: %+v execs=%d", res, execs)
	}
	if !strings.Contains(res.Note, "no execute-time fault re-check wired") {
		t.Fatalf("the refusal must name the missing control, got %q", res.Note)
	}
}
