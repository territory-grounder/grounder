package faultinjector

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TWO FAIL-OPENS IN THE ONE TOOL THAT MUTATES PRODUCTION BY DESIGN.
//
// Both closed a durable restore obligation on evidence that was never gathered, and the close is PERMANENT:
// Outstanding() selects only 'pending'/'failed', so a wrongly-closed row is never revisited. The consequence
// is the exact stranding this package's ledger exists to prevent — a guest left broken while the record
// asserts it was restored, and busyHosts releasing the quarantine so another fault can stack on top.
//
// The shared root cause is SSHRunner's contract: "a non-zero remote exit is DATA, not a transport failure",
// so it returns a NIL ERROR for any non-zero exit. The ssh client reports an unreachable host as 255, and a
// CommandContext kill surfaces as -1. Both arrive as (out, code, nil) and were read as remote answers.

// EXIT 255 IS NOT AN ANSWER. `code != 0` meant an unreachable guest — the state in which a fill is MOST likely
// still present — reported the fill as removed.
func TestVerifyFillRemoved_UnreachableGuestIsUnknownNotRemoved(t *testing.T) {
	for name, code := range map[string]int{
		"ssh cannot connect (255)": 255,
		"context kill (-1)":        -1,
		"command not found (127)":  127,
		"permission denied (126)":  126,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			f.code["test"] = code
			ok, err := VerifyFillRemoved(context.Background(), f, "dc1openwebui01", FillPath)
			if err == nil {
				t.Fatalf("exit %d is NOT proof the fill was removed — it must fail closed with an error", code)
			}
			if ok {
				t.Fatalf("exit %d reported the fill as REMOVED — this is the fail-open that strands a guest", code)
			}
			if !strings.Contains(err.Error(), "not proof of removal") {
				t.Errorf("the error must say why it is not proof, got %q", err)
			}
		})
	}
}

// Only `test -e` exit 1 proves absence, and exit 0 proves presence. Without these the fix could have been
// "always return an error", which would strand every obligation the other way.
func TestVerifyFillRemoved_OnlyExitOneProvesAbsence(t *testing.T) {
	f := newFake()
	f.code["test"] = 1
	ok, err := VerifyFillRemoved(context.Background(), f, "h", FillPath)
	if err != nil || !ok {
		t.Fatalf("exit 1 means the path is absent — the fill IS removed; got ok=%v err=%v", ok, err)
	}

	f2 := newFake()
	f2.code["test"] = 0
	ok2, err2 := VerifyFillRemoved(context.Background(), f2, "h", FillPath)
	if err2 != nil || ok2 {
		t.Fatalf("exit 0 means the path EXISTS — the fill is still there; got ok=%v err=%v", ok2, err2)
	}
}

// The sibling verifiers were always fail-closed; this pins the symmetry so VerifyFillRemoved cannot drift back
// into being the odd one out.
func TestEveryVerifierFailsClosedOnAnUnreachableHost(t *testing.T) {
	f := newFake()
	f.code["docker"], f.code["pct"], f.code["test"] = 255, 255, 255

	if ok, err := VerifyContainerRunning(context.Background(), f, "h", "mealie"); err == nil || ok {
		t.Error("VerifyContainerRunning must fail closed on exit 255")
	}
	if ok, err := VerifyGuestRunning(context.Background(), f, "pve01", "101"); err == nil || ok {
		t.Error("VerifyGuestRunning must fail closed on exit 255")
	}
	if ok, err := VerifyFillRemoved(context.Background(), f, "h", FillPath); err == nil || ok {
		t.Error("VerifyFillRemoved must fail closed on exit 255 — it was the only one that did not")
	}
}

// AN AMBIGUOUS INJECTION FAILURE IS NOT PROOF NOTHING BROKE. `docker stop` that times out may already have
// stopped the container; closing the obligation there is how a stopped service is recorded as restored.
func TestAmbiguousInjectionFailureIsNotPreEffect(t *testing.T) {
	f := newFake()
	f.code["docker"] = 255 // transport lost — the stop may or may not have committed
	err := InjectContainerDown(context.Background(), f, "dc1mealie01", "mealie")
	if err == nil {
		t.Fatal("a 255 from docker stop must be an error")
	}
	if errors.Is(err, ErrPreEffect) {
		t.Fatal("a transport failure is AMBIGUOUS — marking it pre-effect closes the obligation and strands " +
			"a container that may well be stopped")
	}
}

// A refused PRECONDITION is provably pre-effect: nothing ran, so the obligation is genuinely void and closing
// it early is correct. Without this the fix would quarantine healthy hosts forever on every refusal.
func TestRefusedPreconditionIsPreEffect(t *testing.T) {
	f := newFake()
	err := InjectContainerDown(context.Background(), f, "dc1mealie01", "")
	if err == nil {
		t.Fatal("an undeclared container must abort")
	}
	if !errors.Is(err, ErrPreEffect) {
		t.Fatalf("a refusal that never reached the host IS pre-effect — its obligation may close; got %q", err)
	}
	if len(f.calls) != 0 {
		t.Fatal("a pre-effect refusal must not have run anything")
	}
}
