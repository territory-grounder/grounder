package desc

import (
	"strings"
	"testing"
)

// The shipped descriptors must validate; these cover the refusals that keep a dialog from lying.
func TestValidateRefusesADishonestDescriptor(t *testing.T) {
	ok := Descriptor{
		Surface: "notifier", SourceType: "probe", Title: "Probe",
		Fields: []Field{{Name: "url", EnvKey: "TG_PROBE_URL", Type: TypeURL, Security: SecOrdinary, Effect: EffectLive}},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a well-formed descriptor was refused: %v", err)
	}

	// A settable secret POINTER would move the credential out from under the boot secret-policy gate.
	bad := ok
	bad.Fields = []Field{{Name: "ref", EnvKey: "TG_PROBE_REF", Type: TypeSecretRef, Security: SecOrdinary, Effect: EffectLive}}
	if err := bad.Validate(); err == nil {
		t.Error("a settable secret reference was accepted")
	}

	// A MUTATING test is an unreviewed action triggered from a settings dialog.
	bad = ok
	bad.Test = TestSpec{Verb: "restart the guest", Mutating: true}
	if err := bad.Validate(); err == nil {
		t.Error("a mutating test was accepted")
	}

	// A module claiming a control-plane namespace would put governance behind a connector's form.
	bad = ok
	bad.Fields = []Field{{Name: "safety.may_actuate", Type: TypeBool, Security: SecOrdinary, Effect: EffectLive}}
	if err := bad.Validate(); err == nil {
		t.Error("a module claimed a control-plane namespace")
	}
}

// A MODULE MAY NOT NAME ITS OWN SECRET PATH.
//
// A descriptor that could declare a path could point the console's writer at another module's secret — or
// at TG's own — and the writer is necessarily allowed to write somewhere. Deriving the lane makes that
// unreachable rather than merely discouraged.
//
// KILLING MUTATION: drop the derived-path check in Validate. RED.
func TestAModuleCannotNameItsOwnSecretPath(t *testing.T) {
	base := func(path string) Descriptor {
		return Descriptor{
			Surface: "notifier", SourceType: "probe", Title: "Probe",
			Fields: []Field{{Name: "token", Type: TypeSecretValue, Security: SecSecret, Effect: EffectLive}},
			Secret: SecretLane{KVPath: path, Field: "token"},
		}
	}
	// The derived lane is accepted.
	if err := base(ModuleSecretPath("notifier", "probe")).Validate(); err != nil {
		t.Fatalf("the derived lane was refused: %v", err)
	}
	// Anything else is refused — including another module's lane and TG's own namespace.
	for _, bad := range []string{
		"secret/data/tg/modules/notifier/matrix", // a DIFFERENT module's lane
		"secret/data/tg/matrix",                  // TG's own operational namespace
		"secret/data/tg/modules/../../root",      // traversal
	} {
		if err := base(bad).Validate(); err == nil {
			t.Errorf("a descriptor named its own secret path %q and was accepted", bad)
		}
	}
}

// Every derived lane must sit under the prefix the writer's policy is scoped to, or the Save will 403.
func TestDerivedLanesSitUnderTheWriterPrefix(t *testing.T) {
	for _, tc := range [][2]string{{"notifier", "matrix"}, {"tracker", "youtrack"}, {"cmdb", "netbox"}} {
		got := ModuleSecretPath(tc[0], tc[1])
		if !strings.HasPrefix(got, ModuleSecretPrefix) {
			t.Errorf("%s/%s derives %q, outside the writer's scope", tc[0], tc[1], got)
		}
	}
}
