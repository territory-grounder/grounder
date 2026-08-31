package selftest

import (
	"context"
	"strings"
	"testing"
)

type okTester struct{}

func (okTester) SelfTest(context.Context, string) (Result, error) { return Result{Summary: "ok"}, nil }

type ptrTester struct{}

func (*ptrTester) SelfTest(context.Context, string) (Result, error) { return Result{}, nil }

// A TYPED NIL MUST NOT LOOK LIKE A WORKING PROBE.
//
// `any(v).(Tester)` succeeds for a nil *ptrTester — the interface carries a type, so it is non-nil and a
// `t != nil` guard reads as if it worked. Registering that as a probe turns "this module is not
// configured" into a panic inside the activity when an operator presses TEST.
//
// KILLING MUTATION: delete the reflect switch in Of. RED.
func TestATypedNilIsNotAProbe(t *testing.T) {
	var nilPtr *ptrTester
	if got, ok := Of(any(nilPtr)); ok {
		t.Fatalf("a typed-nil module was accepted as a Tester (%v) — pressing TEST would panic instead of "+
			"reporting that no test is implemented", got)
	}
	// The non-nil case must still be accepted, or the guard has simply disabled the feature.
	if _, ok := Of(any(&ptrTester{})); !ok {
		t.Fatal("a real Tester was rejected — the nil guard is refusing everything")
	}
	if _, ok := Of(any(okTester{})); !ok {
		t.Fatal("a value-receiver Tester was rejected")
	}
	if _, ok := Of(any("not a tester")); ok {
		t.Fatal("a non-Tester was accepted")
	}
}

// The probe body must be unmistakable and must name who caused it.
//
// KILLING MUTATION: drop BodyMarker from ProbeBody. RED.
func TestProbeBodyCannotBeMistakenForAGovernanceDecision(t *testing.T) {
	body := ProbeBody("@ops:example")
	if !strings.Contains(body, BodyMarker) {
		t.Fatalf("the probe body carries no test marker: %q", body)
	}
	if !strings.Contains(body, "@ops:example") {
		t.Errorf("the probe body does not name who triggered it: %q", body)
	}
	// An unnamed operator still produces a marked, attributed message rather than a dangling "by ".
	anon := ProbeBody("   ")
	if !strings.Contains(anon, BodyMarker) || !strings.Contains(anon, "an operator") {
		t.Errorf("an unnamed operator produced %q", anon)
	}
}

func TestOperatorNormalisation(t *testing.T) {
	if got := Operator(""); got != "an operator" {
		t.Errorf("Operator(%q) = %q", "", got)
	}
	if got := Operator(" \t\n"); got != "an operator" {
		t.Errorf("whitespace-only operator normalised to %q", got)
	}
	if got := Operator("@a:b"); got != "@a:b" {
		t.Errorf("a real operator was rewritten to %q", got)
	}
}
