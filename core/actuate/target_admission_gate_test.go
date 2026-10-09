package actuate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// target_admission_gate_test.go — TG-81 b2 gate 4h2. THE DEFECT UNDER TEST: the in-process lease (4h)
// cannot see a sibling process, so two actuation-capable processes could each hold their own lease on ONE
// target; and after a disturbed effect nothing parked the target before the next hand piled on. Every case
// uses the fake effect leaf, so a gate that wrongly PASSED is caught by the execs count.

type fakeAdmission struct {
	mu            sync.Mutex
	refuse        error
	admits        int
	releases      int
	lastTarget    string
	lastRef       string
	lastDisturbed bool
}

func (f *fakeAdmission) Admit(_ context.Context, target, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.admits++
	f.lastTarget, f.lastRef = target, ref
	return f.refuse
}

func (f *fakeAdmission) Release(_ context.Context, target, ref string, disturbed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
	f.lastTarget, f.lastRef, f.lastDisturbed = target, ref, disturbed
}

// failingActuator fires and fails — the "disturbed effect" the cooldown exists for.
type failingActuator struct {
	fakeActuator
	err      error
	exitCode int
}

func (a *failingActuator) Exec(ctx context.Context, argv []string, stdin []byte) (actuation.Result, error) {
	a.fakeActuator.execs++
	if a.err != nil {
		return actuation.Result{}, a.err
	}
	return actuation.Result{ExitCode: a.exitCode}, nil
}

func admissionInterceptor(act actuation.Actuator, adm TargetAdmission) *Interceptor {
	return actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).WithTargetAdmission(adm)
}

// A refused durable admission must refuse BEFORE the effect and carry the store's own reason — a held
// claim, a cooldown, and an unreachable store are all this same refusal (fail closed, h-ssh inverted).
// KILLING MUTATION: drop the refusal branch in gate 4h2 (admit unconditionally) — the execs assertion
// fails (the effect fired through a held claim).
func TestTargetAdmissionRefusalBlocksTheEffect(t *testing.T) {
	act := &fakeActuator{}
	adm := &fakeAdmission{refuse: errors.New(`target "web01" is claimed by in-flight session "TG-other"`)}
	i := admissionInterceptor(act, adm)

	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatalf("a refusal must be quiet, not loud: %v", err)
	}
	if !out.Refused {
		t.Fatal("a held claim must refuse")
	}
	if !strings.Contains(out.Reason, "durable target admission refused") || !strings.Contains(out.Reason, "TG-other") {
		t.Fatalf("the refusal must carry the gate name and the store's reason, got %q", out.Reason)
	}
	if act.execs != 0 {
		t.Fatalf("the effect fired through a refused admission (%d execs)", act.execs)
	}
	if adm.releases != 0 {
		t.Fatal("nothing was claimed, so nothing may be released")
	}
}

// A clean execution releases the claim UNDISTURBED — no cooldown for a healthy heal.
func TestCleanExecutionReleasesUndisturbed(t *testing.T) {
	act := &fakeActuator{}
	adm := &fakeAdmission{}
	i := admissionInterceptor(act, adm)

	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if out.Refused || act.execs != 1 {
		t.Fatalf("the admissible request must execute once, got refused=%v execs=%d (%s)", out.Refused, act.execs, out.Reason)
	}
	if adm.admits != 1 || adm.releases != 1 {
		t.Fatalf("claim/release must pair: admits=%d releases=%d", adm.admits, adm.releases)
	}
	if adm.lastDisturbed {
		t.Fatal("a clean execution must not start a cooldown")
	}
	if adm.lastTarget != "web01" {
		t.Fatalf("the claim must key on the action target, got %q", adm.lastTarget)
	}
}

// A disturbed effect — transport error or non-zero exit — releases WITH the cooldown flag. KILLING
// MUTATION: drop either `execDisturbed = true` in the execute failure paths — the matching case fails.
func TestDisturbedEffectReleasesWithCooldown(t *testing.T) {
	cases := []struct {
		name string
		act  *failingActuator
	}{
		{"transport error", &failingActuator{err: errors.New("ssh: broken pipe mid-restart")}},
		{"non-zero exit", &failingActuator{exitCode: 42}},
	}
	for _, c := range cases {
		adm := &fakeAdmission{}
		i := admissionInterceptor(c.act, adm)
		out, err := i.Do(context.Background(), goodRequest(t))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !out.Refused {
			t.Fatalf("%s: a failed effect is a refusal, got %+v", c.name, out)
		}
		if adm.releases != 1 || !adm.lastDisturbed {
			t.Fatalf("%s: the claim must release disturbed (releases=%d disturbed=%v)", c.name, adm.releases, adm.lastDisturbed)
		}
	}
}

// A refusal BETWEEN admission and the effect — the baseline gate, with both arms unestablished — releases
// the claim UNDISTURBED: only the effect itself can disturb a target, and a claim that outlived a
// pre-effect refusal would park the target for nothing.
func TestPreEffectRefusalReleasesUndisturbed(t *testing.T) {
	act := &fakeActuator{}
	adm := &fakeAdmission{}
	i := admissionInterceptor(act, adm)

	r := goodRequest(t)
	r.Observe = func(context.Context) ([]verify.ObservedAlert, bool) { return nil, false } // pair arm unestablished
	r.PreAnomalous = nil                                                                  // host arm absent

	out, err := i.Do(context.Background(), r)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !out.Refused || act.execs != 0 {
		t.Fatalf("the baseline gate must refuse pre-effect, got refused=%v execs=%d (%s)", out.Refused, act.execs, out.Reason)
	}
	if adm.admits != 1 || adm.releases != 1 || adm.lastDisturbed {
		t.Fatalf("a pre-effect refusal must release undisturbed: admits=%d releases=%d disturbed=%v",
			adm.admits, adm.releases, adm.lastDisturbed)
	}
}
