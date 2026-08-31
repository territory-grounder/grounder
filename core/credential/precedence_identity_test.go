package credential

import (
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// A TIE IS ONLY AMBIGUOUS IF THE ANSWERS DIFFER (REQ-1606a).
//
// Equal specificity means two rules have equal CLAIM on a target, not that they disagree. A source deriving
// rules from an external registry routinely emits several for one host: the AWX source makes one group rule
// per INVENTORY, and AWX binds credentials to job templates rather than to hosts, so a host legitimately in
// two credentialed inventories produces two rules BY CONSTRUCTION. No further querying collapses that — AWX
// does not store "the credential for host X".
//
// Measured live: 7 hosts — every Proxmox node plus the Nextcloud pair — were uninvestigable for exactly this
// reason, and both tied rules resolved to the SAME user and SAME key ref, because only one AWX credential is
// mapped to a TG SecretRef at all. TG was blind on its own hypervisors to protect a choice it never had.

func bundleWith(t *testing.T, user, keyRef string) Bundle {
	t.Helper()
	b, err := NewBundle(BundleSpec{User: user, Port: 22, Scheme: SchemeSSH, SSHKeyRef: config.SecretRef(keyRef)})
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	return b
}

// THE FIX: two equal-specificity rules naming the SAME identity resolve, and do so deterministically.
func TestEqualSpecificitySameIdentityResolves(t *testing.T) {
	same := func() []Rule {
		return []Rule{
			{ID: "cert-sync", Selector: Selector{KindGroup, "Cert Sync Targets"}, Bundle: bundleWith(t, "root", "file:/secrets/one_key")},
			{ID: "proxmox-dyn", Selector: Selector{KindGroup, "Proxmox Dynamic Inventory"}, Bundle: bundleWith(t, "root", "file:/secrets/one_key")},
		}
	}
	got, err := selectRule(same(), Target{Host: "dc1nc02", Groups: []string{"Cert Sync Targets", "Proxmox Dynamic Inventory"}})
	if err != nil {
		t.Fatalf("two rules naming ONE identity must resolve, not refuse: %v", err)
	}
	if got.Bundle.User() != "root" || got.Bundle.SSHKeyRef() != config.SecretRef("file:/secrets/one_key") {
		t.Fatalf("wrong identity resolved: user=%q key=%q", got.Bundle.User(), got.Bundle.SSHKeyRef())
	}
	// Deterministic: the audit must record a stable winner across runs, not whichever rule was seen first.
	for i := 0; i < 5; i++ {
		again, err := selectRule(same(), Target{Host: "dc1nc02", Groups: []string{"Cert Sync Targets", "Proxmox Dynamic Inventory"}})
		if err != nil || again.ID != got.ID {
			t.Fatalf("winner must be stable across runs: first=%q now=%q err=%v", got.ID, again.ID, err)
		}
	}
}

// THE PROPERTY THAT MUST SURVIVE: genuinely different identities still refuse. Guessing between two real
// candidates is an ungoverned access path — the case fail-closed exists for.
func TestEqualSpecificityDifferentIdentityStillRefuses(t *testing.T) {
	for name, other := range map[string]Bundle{
		"different key":  bundleWith(t, "root", "file:/secrets/other_key"),
		"different user": bundleWith(t, "svc", "file:/secrets/one_key"),
	} {
		t.Run(name, func(t *testing.T) {
			rules := []Rule{
				{ID: "a", Selector: Selector{KindGroup, "G1"}, Bundle: bundleWith(t, "root", "file:/secrets/one_key")},
				{ID: "b", Selector: Selector{KindGroup, "G2"}, Bundle: other},
			}
			_, err := selectRule(rules, Target{Host: "h", Groups: []string{"G1", "G2"}})
			if !errors.Is(err, ErrAmbiguous) || !IsRefused(err) {
				t.Fatalf("a tie between DIFFERENT identities must still fail closed, got %v", err)
			}
		})
	}
}

// A more specific rule still wins outright — the specificity ladder is untouched by this change.
func TestMoreSpecificRuleStillBeatsATie(t *testing.T) {
	rules := []Rule{
		{ID: "g1", Selector: Selector{KindGroup, "G1"}, Bundle: bundleWith(t, "root", "file:/a")},
		{ID: "g2", Selector: Selector{KindGroup, "G2"}, Bundle: bundleWith(t, "root", "file:/b")},
		{ID: "host", Selector: Selector{KindHost, "h"}, Bundle: bundleWith(t, "exact", "file:/c")},
	}
	got, err := selectRule(rules, Target{Host: "h", Groups: []string{"G1", "G2"}})
	if err != nil {
		t.Fatalf("an exact-host rule must win over a group tie: %v", err)
	}
	if got.ID != "host" {
		t.Fatalf("want the exact-host rule to win, got %q", got.ID)
	}
}

// MUTATION CONTROL (partial — see precedence_identity_closedset_test.go for the complete one).
//
// ★ THIS COMMENT USED TO CLAIM "every field that decides WHO connects and WITH WHAT" AND THEN TEST TWO of
// six. Measured by deleting each comparison in sameIdentity and running the whole package: Port, Scheme and
// APITokenRef were all deletable with the suite GREEN. A control whose stated scope exceeds its coverage is
// worse than a narrow one honestly labelled, because the claim is what a reviewer reads.
// This case is kept as the readable smoke test for the two fields it does cover; the closed-set control
// derives the full field list from sameIdentity's own source, so a seventh field cannot arrive uncovered.
func TestMutationControl_SameIdentityDiscriminatesOnEveryField(t *testing.T) {
	base := bundleWith(t, "root", "file:/k")
	if !sameIdentity(base, bundleWith(t, "root", "file:/k")) {
		t.Fatal("identical specs must compare equal, or the fix never engages")
	}
	for name, other := range map[string]Bundle{
		"user": bundleWith(t, "other", "file:/k"),
		"key":  bundleWith(t, "root", "file:/other"),
	} {
		if sameIdentity(base, other) {
			t.Errorf("%s differs but compared EQUAL — a different principal would be silently accepted", name)
		}
	}
	if sameIdentity(Bundle{}, base) || sameIdentity(base, Bundle{}) {
		t.Error("an invalid bundle must never compare equal to a valid one")
	}
}
