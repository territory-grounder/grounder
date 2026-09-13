package ssh

import (
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/safety"
)

// REACHABILITY. A verb the registry declares and this leaf refuses is not a verb, it is a rumour — and the
// refusal carries no signal, because "not allowlisted" reads identically whether the class is deliberately
// out of scope or simply missing from a second list nobody remembered to update.

func TestEveryRegisteredSSHClassIsReachableOrDeclared(t *testing.T) {
	// The families this leaf has runtime vocabulary for. Adding a family to the registry without adding it here
	// is the failure this test reports.
	knownFamilies := map[string]bool{
		opschema.FamilyServiceLifecycle:   true,
		opschema.FamilyContainerLifecycle: true,
	}
	// Built through the REAL constructor, not a struct literal. An earlier version of this test seeded
	// `reversible` itself, which proved reversibleFromRegistry() was correct while proving nothing about
	// whether the wired path calls it — the same one-level-up version of the very gap this test exists for.
	m := New("host01", "svc-agent", &fakeRunner{},
		WithMutation(safety.NewActuatingChokepoint(), []string{"some.service"}, []string{"some-container"}))

	checked := 0
	for _, s := range opschema.Specs() {
		if s.Kind() != opschema.EffectSSHArgv || len(s.ArgvTemplate) == 0 {
			continue
		}
		checked++
		var param string
		for _, p := range s.Params {
			if p.Required {
				param = p.Name
				break
			}
		}
		target := sampleFor(param)
		cmd, back, err := m.resolveOp(s.OpClass, target)

		if !knownFamilies[s.Family] {
			if err == nil {
				t.Errorf("%s is in family %q, which this leaf has NO vocabulary for, yet it resolved — the leaf "+
					"cannot know what its slot is or which allowlist gates it", s.OpClass, s.Family)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s is a registered ssh-argv class in a known family but does NOT resolve at the leaf "+
				"(%v) — it is registered, classifiable and INERT", s.OpClass, err)
			continue
		}
		if len(cmd) == 0 {
			t.Errorf("%s resolved to an empty argv", s.OpClass)
		}
		if len(back) == 0 {
			t.Errorf("%s resolved with NO rollback — INV-07 requires a bound compensating action", s.OpClass)
		}
	}
	if checked == 0 {
		t.Fatal("no ssh-argv templated class in the registry — this oracle would pass vacuously")
	}
}

func TestAStartVerbRollsBackToItsInverseNotToItself(t *testing.T) {
	m := New("host01", "svc-agent", &fakeRunner{},
		WithMutation(safety.NewActuatingChokepoint(), []string{"some.service"}, []string{"some-container"}))
	for _, c := range []struct{ class, target, wantVerb string }{
		{OpClassStartService, "some.service", "stop"},
		{OpClassStartContainer, "some-container", "stop"},
	} {
		fwd, back, err := m.resolveOp(c.class, c.target)
		if err != nil {
			t.Errorf("%s: %v", c.class, err)
			continue
		}
		if len(back) < 2 || back[1] != c.wantVerb {
			t.Errorf("%s forward=%v rollback=%v — a start's compensating action must be %q, never another start",
				c.class, fwd, back, c.wantVerb)
		}
	}
	// and the idempotent verbs must still roll back to a re-run
	for _, c := range []struct{ class, target string }{
		{OpClassRestartService, "some.service"},
		{OpClassReloadService, "some.service"},
		{OpClassRestartContainer, "some-container"},
	} {
		fwd, back, err := m.resolveOp(c.class, c.target)
		if err != nil {
			t.Errorf("%s: %v", c.class, err)
			continue
		}
		if len(fwd) != len(back) || fwd[1] != back[1] {
			t.Errorf("%s forward=%v rollback=%v — an idempotent verb reconverges by re-running", c.class, fwd, back)
		}
	}
}

func TestAFamilyTheLeafDoesNotUnderstandNeverResolves(t *testing.T) {
	// start-guest is genuinely registered and genuinely in a family this leaf has no vocabulary for
	// (guest-lifecycle — it routes to the proxmox lane). Seeding it into the resolvable set is the only way to
	// ask "and if it got this far?".
	spec, ok := opschema.Lookup("start-guest")
	if !ok {
		// NOT a skip. opschema.Lookup reads the EMBEDDED, code-released registry, so start-guest being absent
		// is a regression in the thing this oracle exists to guard — and a skip would convert that regression
		// into a silent pass, which is the "green that ran nothing" shape. Fail loudly instead.
		t.Fatal("start-guest is not registered in the embedded op-class registry — this oracle's premise is gone, which is a regression, not a reason to skip")
	}
	if spec.Family == opschema.FamilyServiceLifecycle || spec.Family == opschema.FamilyContainerLifecycle {
		t.Fatalf("start-guest is now in family %q, which this leaf DOES understand — pick another probe or this "+
			"test proves nothing", spec.Family)
	}
	m := New("host01", "svc-agent", &fakeRunner{},
		WithMutation(safety.NewActuatingChokepoint(), []string{"some-guest"}, []string{"some-guest"}))
	m.reversible = map[string]bool{"start-guest": true} // seeded PAST the first guard

	cmd, _, err := m.resolveOp("start-guest", "some-guest")
	if err == nil {
		t.Fatalf("a %q class resolved at the ssh leaf to %v — this leaf cannot know whether that slot is a "+
			"unit, a container or a guest, nor which allowlist gates it", spec.Family, cmd)
	}
	// WHICH refusal matters. ErrNoExecutionPath says "this leaf does not do that kind of thing"; anything else
	// says the leaf TRIED, got as far as building an argv, and failed for an incidental reason — which is the
	// difference between a class being out of scope and a class being one param-name away from resolving
	// without ever meeting an allowlist. Asserting the specific error is also the only way this oracle can
	// discriminate, since a probe class in an unknown family fails a param lookup either way.
	if !errors.Is(err, ErrNoExecutionPath) {
		t.Errorf("a %q class was refused with %v, want ErrNoExecutionPath — the family switch must reject it "+
			"OUTRIGHT, not fall through to another family's vocabulary and fail incidentally", spec.Family, err)
	}
}
