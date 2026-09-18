package cisco

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/actuation"
)

// THE HOLE THIS CLOSES. The slice-1 guard admits any first-token `show` carrying no mutating word — so
// `show running-config` was admitted, and on ASA/IOS that output is the box's credential store (IPsec
// pre-shared keys, SNMP communities, RADIUS/TACACS keys, local password hashes). Reading a secret is not a
// mutation, so nothing in the mutating-verb guard refused it. These pin the floor that does.
func TestTheCredentialFloorRefusesSecretReads(t *testing.T) {
	refused := [][]string{
		{"show", "running-config"},                    // the whole config = the credential store
		{"show", "startup-config"},                    // same, from flash
		{"show", "running-config", "all"},             // `all` re-widens a "scoped" dump
		{"show", "running-config", "crypto"},          // the section that IS the secret
		{"show", "running-config", "tunnel-group"},    // pre-shared keys live here
		{"show", "running-config", "snmp-server"},     // community strings
		{"show", "crypto", "ikev1", "pre-shared-key"}, // prints keys outright
		{"show", "running-config", "aaa-server"},      // RADIUS/TACACS keys
		{"show", "tech-support"},                      // bundles the running-config
		{"more", "system:running-config"},             // ASA's file-read back door
		{"show", "username"},                          // local accounts
		{"show", "snmp-server", "community"},          // community strings
	}
	for _, argv := range refused {
		if why := RefuseCredentialBearing(argv); why == "" {
			t.Errorf("%q must be refused by the credential floor", strings.Join(argv, " "))
		}
	}
}

// SCOPE-AWARENESS IS THE POINT, not a blanket ban: a section-scoped config read is a real diagnostic ("what is
// actually configured on the link that died") and the read slice's own oracle pins it as admitted. A floor
// that deleted it would close a hole by removing a capability.
func TestTheCredentialFloorAdmitsScopedNonSecretReads(t *testing.T) {
	admitted := [][]string{
		{"show", "running-config", "interface", "GigabitEthernet0/1"},
		{"show", "running-config", "access-list"},
		{"show", "running-config", "route"},
		{"show", "interface"},
		{"show", "crypto", "ipsec", "sa"}, // SA state, not key material
		{"show", "access-list"},
		{"ping", "192.0.2.1"},
	}
	for _, argv := range admitted {
		if why := RefuseCredentialBearing(argv); why != "" {
			t.Errorf("%q must still be admitted, got refusal: %s", strings.Join(argv, " "), why)
		}
	}
}

// The floor runs on the MODULE's Exec path with no catalog wired at all — it is not something a catalog
// switches on. KILLING MUTATION: delete the RefuseCredentialBearing call in Exec and this goes red.
func TestExecRefusesACredentialReadWithNoCatalog(t *testing.T) {
	run := &recordingRunner{}
	m := New(run)
	if _, err := m.Exec(context.Background(), []string{"show", "running-config"}, nil); err == nil {
		t.Fatal("an unscoped config dump must be refused even with no catalog wired")
	}
	if run.calls != 0 {
		t.Fatalf("the device must never be reached for a credential read (calls=%d)", run.calls)
	}
}

// A catalog NARROWS admission and can never widen it: an improvised diagnostic the verb guard would have
// allowed is refused by name once a catalog is installed.
func TestACatalogNarrowsAdmission(t *testing.T) {
	cat, err := NewCatalog(DefaultCatalog(), PlatformASA)
	if err != nil {
		t.Fatalf("default catalog must build: %v", err)
	}
	run := &recordingRunner{}
	m := New(run).WithCatalog(cat)

	// Catalogued: admitted, and reaches the device exactly as declared.
	if _, err := m.Exec(context.Background(), []string{"show", "xlate"}, nil); err != nil {
		t.Fatalf("a catalogued diagnostic must be admitted: %v", err)
	}
	if run.calls != 1 || run.last != "show xlate" {
		t.Fatalf("device got %q (calls=%d), want \"show xlate\"", run.last, run.calls)
	}
	// Not catalogued, yet verb-guard-clean: refused BY THE CATALOG.
	before := run.calls
	if _, err := m.Exec(context.Background(), []string{"show", "inventory"}, nil); err == nil {
		t.Fatal("an uncatalogued show must be refused once a catalog is installed")
	}
	if run.calls != before {
		t.Fatal("an uncatalogued show reached the device")
	}
	// And the same module WITHOUT a catalog still admits it — proving the catalog is what narrowed it, and
	// that this slice is behaviour-preserving until one is wired.
	if _, err := New(&recordingRunner{}).Exec(context.Background(), []string{"show", "inventory"}, nil); err != nil {
		t.Fatalf("with no catalog the verb guard alone must still admit it (behaviour-preserving): %v", err)
	}
}

// An entry with no Params admits ONLY its exact argv — extra tokens are caller input the entry never declared.
// An entry WITH Params admits its prefix plus arguments.
func TestCatalogParamDiscipline(t *testing.T) {
	cat, err := NewCatalog(DefaultCatalog(), PlatformASA)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.Admits([]string{"show", "xlate", "detail"}); err == nil {
		t.Error("a no-Params entry must not admit extra tokens")
	}
	if err := cat.Admits([]string{"show", "route", "192.0.2.1"}); err != nil {
		t.Errorf("a Params entry must admit its argument: %v", err)
	}
}

// NewCatalog FAILS CLOSED on a set it could not honour — and a credential-bearing entry can never be
// catalogued, however it was reviewed. That is the rule that outlives the review.
func TestNewCatalogFailsClosed(t *testing.T) {
	cases := map[string][]ShowCommand{
		"empty set":        {},
		"no name":          {{Argv: []string{"show", "version"}, Platform: PlatformAny}},
		"no argv":          {{Name: "x", Platform: PlatformAny}},
		"not a read verb":  {{Name: "x", Argv: []string{"configure", "terminal"}, Platform: PlatformAny}},
		"credential entry": {{Name: "dump", Argv: []string{"show", "running-config"}, Platform: PlatformAny}},
		"duplicate name": {
			{Name: "v", Argv: []string{"show", "version"}, Platform: PlatformAny},
			{Name: "v", Argv: []string{"show", "interface"}, Platform: PlatformAny},
		},
	}
	for name, set := range cases {
		if _, err := NewCatalog(set, PlatformAny); err == nil {
			t.Errorf("%s: NewCatalog must fail closed", name)
		}
	}
	// A set that applies to NO entry for the requested platform admits nothing — also fail closed.
	if _, err := NewCatalog([]ShowCommand{{Name: "asaonly", Argv: []string{"show", "xlate"}, Platform: PlatformASA}}, PlatformIOS); err == nil {
		t.Error("a catalog with no entry for the platform must fail closed")
	}
}

// The shipped default set must itself satisfy the floor — a catalog cannot ship a secret-reading command.
func TestDefaultCatalogCarriesNoCredentialRead(t *testing.T) {
	cmds := DefaultCatalog()
	if len(cmds) == 0 {
		t.Fatal("vacuity floor: the default catalog is empty, so this asserts nothing")
	}
	for _, c := range cmds {
		if why := RefuseCredentialBearing(c.Argv); why != "" {
			t.Errorf("shipped catalog entry %q is credential-bearing: %s", c.Name, why)
		}
		if c.Why == "" {
			t.Errorf("entry %q has no Why — a catalog review should read as diagnosis, not trivia", c.Name)
		}
	}
}

// recordingRunner captures what the module handed the transport.
type recordingRunner struct {
	calls int
	last  string
}

func (r *recordingRunner) RunShow(_ context.Context, cmd string) (actuation.Result, error) {
	r.calls++
	r.last = cmd
	return actuation.Result{}, nil
}

// Bounded Admits (2026-08-25): a param-bearing entry admits AT MOST its declared parameter count beyond
// its fixed argv — the unbounded form accepted arbitrarily longer vectors, unreachable through the read
// tool but load-bearing for the write lane's guard. Revert the bound and this arm goes red.
func TestAdmitsBoundsExtraTokensToDeclaredParams(t *testing.T) {
	cat, err := NewCatalog(DefaultCatalog(), PlatformASA)
	if err != nil {
		t.Fatal(err)
	}
	var entry ShowCommand
	for _, n := range cat.Names() {
		if e, _ := cat.Lookup(n); len(e.Params) == 1 {
			entry = e
			break
		}
	}
	if entry.Name == "" {
		t.Skip("no single-param ASA entry to bound against")
	}
	ok := append(append([]string(nil), entry.Argv...), "inside")
	if err := cat.Admits(ok); err != nil {
		t.Fatalf("exact argv+1 declared param must admit: %v", err)
	}
	over := append(append([]string(nil), ok...), "surplus-token")
	if err := cat.Admits(over); err == nil {
		t.Fatal("argv with MORE tokens than the entry declares params for must refuse — a guard's bound must not depend on its callers' manners")
	}
}
