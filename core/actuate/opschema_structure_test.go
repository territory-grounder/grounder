package actuate

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
)

// (a) A REGISTERED op-class whose sealed params did not build an argv (restart-service with no `unit`, so
// sealedArgv returned an empty argv) is refused at the STRUCTURE gate with an ACTIONABLE error that names the
// missing param — NOT at execute with an opaque ErrEmptyArgv (the canary's original failure mode). The
// posture is ACTUATING so the mode chokepoint is NOT what refuses; the refusal must come from the structure
// gate (before evidence/policy/mode/execute), and the actuator must never be reached.
func TestInterceptorRefusesMissingRequiredParamAtStructure(t *testing.T) {
	m, err := manifest.New(
		manifest.Action{Target: "web01", OpClass: "restart-service", Op: "restart", Reversible: true}, // NO params.unit
		safety.BandAuto, "plan#nounit", "pred#nounit",
	)
	if err != nil {
		t.Fatalf("seal manifest: %v", err)
	}
	act := &fakeActuator{}
	i := wired(safety.NewActuatingChokepoint(), act) // mutation ON (test-only) — prove it's the STRUCTURE gate, not the mode gate
	out, err := i.Do(context.Background(), Request{
		Manifest: m,
		Gated:    true,
		Argv:     nil, // the sealedArgv output for a restart-service with no unit
		Evidence: boundEvidence(),
		Observe:  noObserved,
		Band:     safety.BandAuto, // fresh band AUTO admits at 1b — isolating the STRUCTURE gate as the refuser
	})
	if err != nil {
		t.Fatalf("Do must record a refusal, not error: %v", err)
	}
	if !out.Refused || act.execs != 0 {
		t.Fatalf("a missing-param action must be refused and NEVER reach the actuator: %+v execs=%d", out, act.execs)
	}
	if !strings.Contains(out.Reason, "structure") || !strings.Contains(out.Reason, opschema.ParamUnit) {
		t.Fatalf("the refusal must be an ACTIONABLE structure-gate reason naming the missing param %q, got %q", opschema.ParamUnit, out.Reason)
	}
	if strings.Contains(out.Reason, "empty argv") {
		t.Fatalf("the refusal must NOT be the opaque execute-time ErrEmptyArgv — it must be caught at propose/structure time; got %q", out.Reason)
	}
}

// The same op-class WITH the unit param present builds a non-empty argv, so the structure gate passes it
// through (it goes on to execute under an actuating posture) — the validator is not stricter than the reader.
func TestInterceptorStructurePassesWhenParamPresent(t *testing.T) {
	m, err := manifest.New(
		manifest.Action{Target: "web01", OpClass: "restart-service", Op: "restart", Params: map[string]string{opschema.ParamUnit: "nginx"}, Reversible: true},
		safety.BandAuto, "plan#unit", "pred#unit",
	)
	if err != nil {
		t.Fatalf("seal manifest: %v", err)
	}
	argv, aerr := opschema.Argv("restart-service", m.Action.Params)
	if aerr != nil {
		t.Fatalf("registry must build the argv: %v", aerr)
	}
	act := &fakeActuator{}
	i := wired(safety.NewActuatingChokepoint(), act)
	out, err := i.Do(context.Background(), Request{
		Manifest: m, Gated: true, Argv: argv, Evidence: boundEvidence(), Observe: noObserved, Band: safety.BandAuto,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !out.Executed || act.execs != 1 {
		t.Fatalf("a complete restart-service action must pass the structure gate and execute: %+v execs=%d", out, act.execs)
	}
}
