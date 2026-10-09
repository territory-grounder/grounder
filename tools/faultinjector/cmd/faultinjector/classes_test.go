package main

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/tools/faultinjector"
)

// A DECLARED CLASS MUST NEVER BE "UNKNOWN" TO THE CLI.
//
// This is the guard for a category, not a string. container-down shipped fully implemented — planner
// eligibility, effect, restore obligation, verified discharge, ten unit tests, a spec requirement — and was
// still unschedulable, because ONE accept-list in the flag parser had never heard of it. The binary crash-
// looped on `unknown class "container-down"` and took the injector down with it; the fault survived only
// because the durable ledger discharged the inherited obligation on restart.
//
// Unit tests of the class's BEHAVIOUR could not catch that: they exercise the planner and the effect directly
// and never go through the CLI. The gap is between "implemented" and "reachable", so the test has to be keyed
// on the closed enumeration rather than on a list a future class would also be missing from.
func TestEveryDeclaredClassIsReachableFromTheCLI(t *testing.T) {
	for _, c := range faultinjector.AllClasses() {
		t.Run(string(c), func(t *testing.T) {
			_, err := faultinjector.ParseClasses(string(c))
			if err == nil {
				return // accepted — schedulable
			}
			// A class MAY be deliberately withheld, but only with a stated reason. "unknown" is not a reason;
			// it means the CLI has simply never been told the class exists.
			if strings.Contains(err.Error(), "unknown class") {
				t.Fatalf("class %q is declared in AllClasses but the CLI rejects it as UNKNOWN — it can never "+
					"be scheduled, and arming it would crash-loop the injector. Add it to parseClasses, or "+
					"reject it explicitly with the reason it is withheld.", c)
			}
			t.Logf("class %q is deliberately withheld: %v", c, err)
		})
	}
}

// The rotation the estate actually runs must parse. A typo here is fatal at boot, which is how the injector
// spent 20 minutes crash-looping.
func TestTheArmedRotationParses(t *testing.T) {
	const armed = "device-down,disk-fill,device-down,container-down,service-down"
	got, err := faultinjector.ParseClasses(armed)
	if err != nil {
		t.Fatalf("the armed rotation must parse: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("want 5 rotation entries, got %d (%v)", len(got), got)
	}
	if got[3] != faultinjector.ClassContainerDown {
		t.Errorf("the 4th slot must be container-down, got %q", got[3])
	}
	// service-down is the slot that unblocks restart-service and start-service, which sat at 1/5 clean runs
	// with nothing in the estate able to provoke them. If this rotation ever drops it, those two op-classes
	// silently stop accruing again and the ladder reports it as "not yet graduated" rather than "unreachable".
	if got[4] != faultinjector.ClassServiceDown {
		t.Errorf("the 5th slot must be service-down, got %q", got[4])
	}
}

// mem-pressure stays withheld, and the refusal must still EXPLAIN itself rather than read as a typo — the
// distinction this whole test file exists to enforce.
func TestWithheldClassRefusesWithAReasonNotAsUnknown(t *testing.T) {
	_, err := faultinjector.ParseClasses(string(faultinjector.ClassMemPressure))
	if err == nil {
		t.Fatal("mem-pressure is deliberately not wired and must be refused")
	}
	if strings.Contains(err.Error(), "unknown class") {
		t.Errorf("a withheld class must say WHY it is withheld, not present as unknown: %v", err)
	}
	if !strings.Contains(err.Error(), "detection rate") {
		t.Errorf("the refusal should carry its measured justification, got %v", err)
	}
}

// A genuine typo is still fatal — the property parseClasses was written for, which must survive this change.
func TestAnActualTypoIsStillFatal(t *testing.T) {
	if _, err := faultinjector.ParseClasses("device-down,contaner-down"); err == nil {
		t.Fatal("a misspelled class must be fatal — otherwise the rotation silently injects less than intended")
	}
}
