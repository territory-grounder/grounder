package cpconfig

import "testing"

// Module keys must be REGISTERED, or a console write of a connector setting is rejected as an unknown key.
// Before this existed the registry held 11 keys, 4 writable, and zero module keys.
func TestModuleKeysBecomeWritableRegistryEntries(t *testing.T) {
	t.Cleanup(func() { SetModuleKeys(nil) })
	SetModuleKeys([]Key{{Name: "module.notifier.matrix.approvers", ConsoleWritable: true}})

	if _, err := ValidateWrite("module.notifier.matrix.approvers", "@a:b"); err != nil {
		t.Fatalf("a registered module key is not writable: %v", err)
	}
	if _, ok := Lookup("module.notifier.matrix.approvers"); !ok {
		t.Fatal("the module key is not in the registry")
	}
}

// THE LAYERING PROPERTY, and the reason this indirection exists at all: a module may not smuggle a key
// into a control-plane namespace. A descriptor claiming "safety.may_actuate" would otherwise put a
// law-pinned governance control behind a connector's settings dialog.
//
// KILLING MUTATION: drop the ModuleKeyPrefix check in SetModuleKeys. RED.
func TestAModuleCannotClaimAControlPlaneKey(t *testing.T) {
	t.Cleanup(func() { SetModuleKeys(nil) })
	SetModuleKeys([]Key{
		{Name: "safety.may_actuate", ConsoleWritable: true},
		{Name: "session.ttl", ConsoleWritable: true},
		{Name: "module.notifier.matrix.rooms", ConsoleWritable: true},
	})
	got := ModuleKeys()
	if len(got) != 1 || got[0].Name != "module.notifier.matrix.rooms" {
		t.Fatalf("a module smuggled a control-plane key into the registry: %+v", got)
	}
	// And the real law key must still be law-pinned and unwritable — the compiled entry, not a shadow.
	k, ok := Lookup("safety.may_actuate")
	if !ok || !k.Law || k.ConsoleWritable {
		t.Fatalf("the compiled law key was displaced by a module's claim: %+v ok=%t", k, ok)
	}
	if _, err := ValidateWrite("safety.may_actuate", "true"); err == nil {
		t.Fatal("the estate mutation master switch became console-writable via a module descriptor")
	}
}

// A module key can never be law-pinned: law lives in the compiled registry, and a module declaring its own
// law would be declaring itself exempt from review.
func TestAModuleKeyIsNeverLawPinned(t *testing.T) {
	t.Cleanup(func() { SetModuleKeys(nil) })
	SetModuleKeys([]Key{{Name: "module.x.y.z", Law: true, ConsoleWritable: true}})
	got := ModuleKeys()
	if len(got) != 1 || got[0].Law {
		t.Fatalf("a module declared itself law-pinned: %+v", got)
	}
}

// The compiled control-plane registry must survive module installation — modules ADD, never replace.
func TestModuleKeysDoNotDisplaceTheCompiledRegistry(t *testing.T) {
	t.Cleanup(func() { SetModuleKeys(nil) })
	before := len(compiledRegistry())
	if before == 0 {
		t.Fatal("vacuity floor: the compiled registry is empty")
	}
	SetModuleKeys([]Key{{Name: "module.a.b.c", ConsoleWritable: true}})
	if got := len(Registry()); got != before+1 {
		t.Fatalf("registry has %d keys, want %d compiled + 1 module", got, before)
	}
	SetModuleKeys(nil)
	if got := len(Registry()); got != before {
		t.Fatalf("clearing module keys left %d keys, want the %d compiled ones", got, before)
	}
}
