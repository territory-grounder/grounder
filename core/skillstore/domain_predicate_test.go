package skillstore

import "testing"

// TG-36 — the store predicate DSL gains Domains. Two properties: unknown domain values are refused at
// WRITE time (so composition never meets an unevaluable predicate), and an existing row's content hash is
// UNCHANGED by the additive field (so no stored version spuriously fails the hash check on upgrade).

func TestValidatePredicateRefusesUnknownDomain(t *testing.T) {
	if err := ValidatePredicate(AppliesWhen{Domains: []string{"kubernetes"}}); err != nil {
		t.Errorf("a known domain was refused: %v", err)
	}
	if err := ValidatePredicate(AppliesWhen{Domains: []string{"windows-server"}}); err == nil {
		t.Error("an unknown domain 'windows-server' was ACCEPTED — a predicate the composer cannot evaluate " +
			"purely would then reach production")
	}
	// The empty-string ("unknown") is not writable — a skill scoped to "unclassified" is a contradiction.
	if err := ValidatePredicate(AppliesWhen{Domains: []string{""}}); err == nil {
		t.Error("the empty domain was accepted as a scope value; empty must mean not-scoped, never a target")
	}
}

// A pre-TG-36 row (no Domains) hashes EXACTLY as it did before the field existed, so upgrading the binary
// does not invalidate every stored skill_version.
func TestContentHashIsUnchangedByAnEmptyDomains(t *testing.T) {
	body := "## some skill body"
	base := AppliesWhen{Phases: []string{"investigate"}, ExecClasses: []string{"deep_investigation"}}
	withNilDomains := base // Domains nil
	if ContentHash(body, base) != ContentHash(body, withNilDomains) {
		t.Fatal("adding the (nil) Domains field changed the content hash of an existing predicate shape")
	}
	// The domain MUST be part of the identity, though: a domain-scoped predicate hashes differently, or
	// two skills differing only by platform would collide.
	scoped := base
	scoped.Domains = []string{"kubernetes"}
	if ContentHash(body, base) == ContentHash(body, scoped) {
		t.Error("a Domains:[kubernetes] predicate hashes identically to one without — the domain is not in " +
			"the content identity, so a k8s and a non-k8s version of the same skill would collide")
	}
}
