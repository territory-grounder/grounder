package actuate

// TG-152 L1: the execute-time param re-check was ASYMMETRIC. The structure-schema gate re-ran
// opschema.ValidateArgs only when len(Argv)==0 — the ssh-argv "argv did not build" defect — but an
// awx-launch effect ALWAYS builds Argv=[LaunchVerb] (len 1), so the one effect kind whose params travel
// OUTSIDE the argv (as AWX extra_vars) was exactly the one whose required-param presence was never
// re-checked at execute. Not a live hole (params are content-hash-sealed from a propose-validated
// proposal), but the propose-time check was the ONLY check — the "present, not reaching" shape again.
//
// KILLING MUTATION (executed 2026-08-11): delete the `else if … EffectAWXLaunch` arm in interceptor.go's
// structure-schema gate — TestAWXLaunchMissingRequiredParamRefusesAtExecute fails ("refused for reason …
// want the schema gate's"), because the request then sails past structure-schema and dies later (or not at
// all) with an unattributable reason. Restore → green. The TG-365 arm: the EMPTY params map must refuse
// identically (missing-everything is the emptiness shape of missing-one).

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
)

// awxLaunchRequest builds a disk-grow request the way production does: the compiled awx-launch effect
// yields Argv=[LaunchVerb] and the params ride the sealed action (to become extra_vars at the leaf).
func awxLaunchRequest(t *testing.T, params map[string]string) Request {
	t.Helper()
	a := manifest.Action{Target: "dc1nc01", OpClass: "disk-grow", Op: "grow", Params: params, Reversible: true}
	m, err := manifest.New(a, safety.BandAuto, "plan-152", "pred-152")
	if err != nil {
		t.Fatal(err)
	}
	r := goodRequest(t)
	r.Manifest = m
	r.Argv = []string{"awx-job-template-launch"} // the LaunchVerb shape every awx-launch effect produces
	return r
}

func TestAWXLaunchMissingRequiredParamRefusesAtExecute(t *testing.T) {
	act := &fakeActuator{}
	i := wired(safety.NewActuatingChokepoint(), act)
	// filesystem present, grow_by MISSING — the exact TG-152 example.
	out, err := i.Do(context.Background(), awxLaunchRequest(t, map[string]string{"filesystem": "/dev/mapper/data"}))
	if err != nil {
		t.Fatalf("failed loud instead of refusing: %v", err)
	}
	if act.execs != 0 || out.Executed {
		t.Fatalf("an awx-launch missing a required param REACHED THE LEAF: %+v execs=%d", out, act.execs)
	}
	if !out.Refused || !strings.Contains(out.Reason, "actuation param schema") {
		t.Fatalf("the refusal must come from the structure-schema gate with the schema's actionable guidance, "+
			"got reason %q — an unattributable later refusal (or none) is the asymmetry TG-152 recorded", out.Reason)
	}
}

// TestAWXLaunchEmptyParamsRefusesAtExecute is the TG-365 emptiness arm of the same gate.
func TestAWXLaunchEmptyParamsRefusesAtExecute(t *testing.T) {
	act := &fakeActuator{}
	i := wired(safety.NewActuatingChokepoint(), act)
	out, err := i.Do(context.Background(), awxLaunchRequest(t, map[string]string{}))
	if err != nil {
		t.Fatalf("failed loud instead of refusing: %v", err)
	}
	if act.execs != 0 || !out.Refused || !strings.Contains(out.Reason, "actuation param schema") {
		t.Fatalf("EMPTY params must refuse at the same gate with the same legibility: %+v execs=%d", out, act.execs)
	}
}

// TestAWXLaunchWithCompleteParamsPassesTheStructureGate is the anti-over-block control: a well-formed
// disk-grow request must NOT be refused by the re-check (later gates may still refuse it for their own
// reasons — asserting only on the structure-schema reason keeps this control honest).
func TestAWXLaunchWithCompleteParamsPassesTheStructureGate(t *testing.T) {
	act := &fakeActuator{}
	i := wired(safety.NewActuatingChokepoint(), act)
	out, err := i.Do(context.Background(), awxLaunchRequest(t, map[string]string{"filesystem": "/dev/mapper/data", "grow_by": "2G"}))
	if err != nil {
		t.Fatalf("failed loud: %v", err)
	}
	if out.Refused && strings.Contains(out.Reason, "actuation param schema") {
		t.Fatalf("a COMPLETE awx-launch param set was refused by the re-check — the symmetric gate must not "+
			"become a stricter validator than the propose path (ACI tolerance): %q", out.Reason)
	}
}