package cisco

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// a valid operator-declared op: an ACL description change, whose undo is the removal of what it added.
func goodOps() []ReversibleOp {
	return []ReversibleOp{{
		Name: "tag-acl", OpClass: "acl-annotate", Platform: PlatformAny,
		Forward: []string{"ip access-list extended OUTSIDE_IN", "remark tg-managed"},
		Inverse: []string{"ip access-list extended OUTSIDE_IN", "no remark tg-managed"},
		Why:     "annotate the ACL TG manages, so a human reading the device knows what owns it",
	}}
}

// RULE 1 — NO OP WITHOUT AN UNDO. The compensating action must exist BEFORE the forward runs.
func TestAnOpWithNoInverseCannotRegister(t *testing.T) {
	ops := goodOps()
	ops[0].Inverse = nil
	if _, err := NewReversibleRegistry(ops, PlatformAny); err == nil {
		t.Fatal("an op with no declared inverse must be refused")
	}
}

// RULE 2 — THE INVERSE MUST NOT BE THE FORWARD. "start rolls back to start" reads as reversible and undoes
// nothing; this estate has hit that trap before.
func TestAnInverseIdenticalToTheForwardIsRefused(t *testing.T) {
	ops := goodOps()
	ops[0].Inverse = append([]string(nil), ops[0].Forward...)
	if _, err := NewReversibleRegistry(ops, PlatformAny); err == nil {
		t.Fatal("an inverse identical to the forward must be refused — it undoes nothing")
	}
}

// RULE 3 — THE NEVER-AUTO FLOOR CLAMPS AT THE LEAF, exactly as the kubernetes leaf clamps delete/drain. A leaf
// that can emit a floor op is one flag away from emitting it automatically.
func TestFloorOpClassesCannotRegister(t *testing.T) {
	floored := []string{"interface-shutdown", "no-interface", "acl-delete", "route-delete", "erase-startup-config", "reboot"}
	for _, class := range floored {
		if !safety.IsNeverAuto(class) {
			t.Fatalf("fixture drift: %q is no longer on the never-auto floor, so this case is vacuous", class)
		}
		ops := goodOps()
		ops[0].OpClass = class
		if _, err := NewReversibleRegistry(ops, PlatformAny); err == nil {
			t.Errorf("op_class %q is on the non-configurable never-auto floor and must not register", class)
		}
	}
	// ...and a non-floor class still registers, so the clamp is not refusing everything.
	if _, err := NewReversibleRegistry(goodOps(), PlatformAny); err != nil {
		t.Fatalf("a non-floor op must still register (anti-vacuity): %v", err)
	}
}

// RULE 4 — PLATFORM IS PART OF THE IDENTITY. IOS says `no shutdown`, ASA says `no shut`; a registry for one
// dialect must never yield the other's verb, which would leave a change applied and un-revertable.
func TestARegistryNeverYieldsTheOtherDialectsOp(t *testing.T) {
	ops := []ReversibleOp{
		{Name: "ios-only", OpClass: "acl-annotate", Platform: PlatformIOS,
			Forward: []string{"ip access-list extended X", "remark a"}, Inverse: []string{"ip access-list extended X", "no remark a"}},
		{Name: "asa-only", OpClass: "acl-annotate", Platform: PlatformASA,
			Forward: []string{"access-list X remark a"}, Inverse: []string{"no access-list X remark a"}},
	}
	ios, err := NewReversibleRegistry(ops, PlatformIOS)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ios.Lookup("asa-only"); ok {
		t.Error("an IOS registry must not yield the ASA op")
	}
	if _, ok := ios.Lookup("ios-only"); !ok {
		t.Error("an IOS registry must yield its own op (anti-vacuity)")
	}
}

// The inverse is the ONE sanctioned place a leading `no` may appear — and only there. A `no` mid-line, or on
// the forward, is still refused, as are persist/reload/mode-escape/separators on either side.
func TestOpLineDisciplineOnBothSides(t *testing.T) {
	cases := map[string]func(o *ReversibleOp){
		"forward leads with no":    func(o *ReversibleOp) { o.Forward = []string{"no remark x"} },
		"forward persists":         func(o *ReversibleOp) { o.Forward = []string{"copy running-config startup-config"} },
		"inverse reloads":          func(o *ReversibleOp) { o.Inverse = []string{"reload"} },
		"inverse has a separator":  func(o *ReversibleOp) { o.Inverse = []string{"no remark x | redirect flash:y"} },
		"forward escapes config":   func(o *ReversibleOp) { o.Forward = []string{"end"} },
		"inverse no mid-line only": func(o *ReversibleOp) { o.Inverse = []string{"remark no shutdown"} },
		"blank line":               func(o *ReversibleOp) { o.Forward = []string{"  "} },
	}
	for name, mutate := range cases {
		ops := goodOps()
		mutate(&ops[0])
		if _, err := NewReversibleRegistry(ops, PlatformAny); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}

// The undo is READ FROM THE REGISTRATION, never derived from the forward at rollback time — which is when
// deriving it is least likely to be right.
func TestInverseIsReadFromTheRegistrationNotDerived(t *testing.T) {
	reg, err := NewReversibleRegistry(goodOps(), PlatformAny)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := reg.InverseOf("tag-acl")
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 2 || inv[1] != "no remark tg-managed" {
		t.Fatalf("the declared inverse must come back verbatim, got %v", inv)
	}
	if _, err := reg.InverseOf("never-registered"); err == nil {
		t.Error("an unregistered name must not yield an inverse")
	}
}

// ExecOp runs a NAMED op — nothing free-form travels this path — and it is still gated by the mode chokepoint.
func TestExecOpRunsOnlyRegisteredOpsAndRespectsTheMode(t *testing.T) {
	reg, err := NewReversibleRegistry(goodOps(), PlatformAny)
	if err != nil {
		t.Fatal(err)
	}
	// At Shadow: refused, device untouched.
	shadowRun := &fakeConfigRunner{}
	shadow := NewWriteModule(shadowRun, safety.NewReadOnlyChokepoint(), []string{"ip access-list "}).WithReversibleOps(reg)
	if _, err := shadow.ExecOp(context.Background(), "tag-acl"); err == nil {
		t.Fatal("ExecOp must refuse at Shadow")
	}
	if shadowRun.calls != 0 {
		t.Fatalf("the device was reached at Shadow (calls=%d)", shadowRun.calls)
	}
	// Armed: the registered forward runs, verbatim.
	run := &fakeConfigRunner{}
	m := NewWriteModule(run, safety.NewActuatingChokepoint(), []string{"ip access-list "}).WithReversibleOps(reg)
	if _, err := m.ExecOp(context.Background(), "tag-acl"); err != nil {
		t.Fatalf("a registered op must run when armed: %v", err)
	}
	if run.calls != 1 || len(run.got) != 2 || run.got[1] != "remark tg-managed" {
		t.Fatalf("the registered forward must reach the device verbatim, got %v", run.got)
	}
	// An UNREGISTERED name is refused even armed — the registry is the allowlist.
	before := run.calls
	if _, err := m.ExecOp(context.Background(), "not-a-registered-op"); err == nil {
		t.Fatal("an unregistered op name must be refused")
	}
	if run.calls != before {
		t.Fatal("an unregistered op reached the device")
	}
	// With NO registry wired, ExecOp refuses rather than falling through to the free-form path.
	bare := NewWriteModule(&fakeConfigRunner{}, safety.NewActuatingChokepoint(), []string{"ip access-list "})
	if _, err := bare.ExecOp(context.Background(), "tag-acl"); err == nil {
		t.Fatal("ExecOp with no registry must refuse (fail closed)")
	}
}

// Adding the registry must NOT widen the free-form path: `no ...` is still refused through Exec.
func TestTheRegistryDoesNotWidenTheFreeFormPath(t *testing.T) {
	reg, _ := NewReversibleRegistry(goodOps(), PlatformAny)
	run := &fakeConfigRunner{}
	m := NewWriteModule(run, safety.NewActuatingChokepoint(), []string{"ip access-list "}).WithReversibleOps(reg)
	if _, err := m.Exec(context.Background(), []string{"ip", "access-list", "extended", "X"}, []byte("no remark tg-managed")); err == nil {
		t.Fatal("a leading `no` must STILL be refused on the free-form path, registry or not")
	}
	if run.calls != 0 {
		t.Fatal("a free-form `no` reached the device")
	}
}

// An empty registry is a wiring error, not a policy: it would admit nothing while looking configured.
func TestAnEmptyRegistryIsRefused(t *testing.T) {
	if _, err := NewReversibleRegistry(nil, PlatformAny); err == nil {
		t.Fatal("an empty op set must be refused")
	}
	if _, err := NewReversibleRegistry([]ReversibleOp{{Name: "x", OpClass: "acl-annotate", Platform: PlatformASA,
		Forward: []string{"a"}, Inverse: []string{"no a"}}}, PlatformIOS); err == nil {
		t.Fatal("a registry with no op for the platform must be refused")
	}
	if _, err := NewReversibleRegistry(goodOps(), PlatformAny); err != nil {
		t.Fatalf("anti-vacuity: a good set must register: %v", err)
	}
}
