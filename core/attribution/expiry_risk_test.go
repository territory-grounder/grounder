package attribution

import (
	"testing"
	"time"
)

// A BOUND THAT DEGRADES THE WRONG WAY IS WORSE THAN NO BOUND, because it fires on a date nobody is watching.
//
// CarveOut's own doc promised "Expiry reverts toward AttributedAuthorized (stand-down, which withholds
// actuation) — the safe direction." That was FALSE on this estate and the comment did not say why.
// Classification precedence is self ▸ carve-out ▸ sanctioned ▸ unsanctioned, so once the window closes an
// actor with no sanctioned entry in its domain falls through to UNSANCTIONED ⇒ AttributedSuspicious ⇒
// security-escalate.
//
// Measured against this same Attribute() before the fix: the live journal carve-out lists the operator's admin
// SSH fingerprint, the journal domain has NO sanctioned principals, and past ValidUntil that key resolved
// attributed-suspicious. Not a stand-down — a SECURITY incident on every ordinary admin login across the
// carve-out's 15 hosts, starting on a known date (2026-10-27, a bound I introduced myself).
//
// These tests assert the DEGRADATION PATH, not the warning text, because the warning is only useful if the
// thing it warns about is real.
func TestExpiryDegradesSafelyOnlyWhenTheActorIsSanctioned(t *testing.T) {
	const opKey = "root!SHA256:operatorkeyfingerprint"
	const selfKey = "root!SHA256:tgactuatorfingerprint"
	from := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)

	build := func(now time.Time, sanctioned bool) Finding {
		s := map[string][]string{}
		if sanctioned {
			s["journal"] = []string{opKey}
		}
		cfg := Config{
			Window: time.Hour, Now: func() time.Time { return now },
			Sanctioned: s,
			SelfActors: map[string]string{"journal": selfKey},
			CarveOuts: []CarveOut{{ID: "pool-ssh", Domain: "journal", Actors: []string{opKey},
				Hosts: []string{"poolhost01"}, ValidFrom: from, ValidUntil: until}},
		}
		ev := []Evidence{{Domain: "journal", Actor: opKey, ActionKind: "ssh-login",
			Target: "poolhost01", ObservedAt: now.Add(-time.Minute), Ref: "j1"}}
		return Attribute("poolhost01", "service-down", ev, nil, cfg)
	}

	during := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	after := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	// 1. INSIDE the window the carve-out governs, sanctioned or not. This is what protects the learning
	//    regime: if sanctioning changed this, the remedy would stop the harness from accruing clean runs.
	if got := build(during, false).Taxonomy; got != AuthorizedTest {
		t.Fatalf("inside the window, unsanctioned: got %v want authorized-test", got)
	}
	if got := build(during, true).Taxonomy; got != AuthorizedTest {
		t.Fatalf("inside the window WITH a sanctioned entry: got %v want authorized-test — sanctioning must be "+
			"inert while the carve-out is valid, or the remedy silently ends the learning regime", got)
	}

	// 2. AFTER expiry the two configurations diverge, and that divergence is the whole finding.
	if got := build(after, false).Taxonomy; got != AttributedSuspicious {
		t.Fatalf("after expiry, unsanctioned: got %v — the defect is that this is attributed-suspicious "+
			"(security-escalate) rather than the stand-down the requirement documents", got)
	}
	if got := build(after, true).Taxonomy; got != AttributedAuthorized {
		t.Fatalf("after expiry WITH a sanctioned entry: got %v want attributed-authorized — this is the "+
			"documented safe direction, and sanctioning is what makes it true", got)
	}
}

// The reporter must name exactly the configurations that degrade unsafely — no more, because a security
// warning that cries wolf is trained away, and no fewer, because the date arrives regardless.
func TestCarveOutExpiryRiskNamesOnlyTheUnsafeConfigurations(t *testing.T) {
	const opKey = "root!SHA256:operator"
	const selfKey = "root!SHA256:tgself"
	const thirdKey = "root!SHA256:thirdparty"
	until := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)

	cfg := Config{
		Sanctioned:       map[string][]string{"pve": {"root@pam"}},
		SelfActors:       map[string]string{"journal": selfKey},
		SanctionedGroups: map[string][]string{"k8s-audit": {"cluster-admins"}},
		CarveOuts: []CarveOut{
			// UNSAFE: neither sanctioned nor self in this domain
			{ID: "journal-pool", Domain: "journal", Actors: []string{opKey}, ValidUntil: until},
			// SAFE: the actor is the platform's own identity there ⇒ attributed-self, never suspicion
			{ID: "journal-self", Domain: "journal", Actors: []string{selfKey}, ValidUntil: until},
			// SAFE: sanctioned in its domain
			{ID: "pve-pool", Domain: "pve", Actors: []string{"root@pam"}, ValidUntil: until},
			// SAFE ENOUGH: a group grant exists for the domain and is resolved by the identity seam at
			// classification time, so this is not reported — over-reporting is its own failure.
			{ID: "k8s-pool", Domain: "k8s-audit", Actors: []string{thirdKey}, ValidUntil: until},
			// UNSAFE, and MIXED: only the uncovered actor is named, because the remedy is per-actor
			{ID: "mixed", Domain: "pve", Actors: []string{"root@pam", thirdKey}, ValidUntil: until},
		},
	}
	got := CarveOutExpiryRisk(cfg)
	byID := map[string][]string{}
	for _, r := range got {
		byID[r.CarveOutID] = r.Actors
	}
	if _, bad := byID["journal-self"]; bad {
		t.Error("the platform's OWN identity was reported as an expiry risk — self resolves attributed-self, not suspicion")
	}
	if _, bad := byID["pve-pool"]; bad {
		t.Error("a sanctioned actor was reported as an expiry risk")
	}
	if _, bad := byID["k8s-pool"]; bad {
		t.Error("a domain with a group grant was reported — the seam resolves groups at classification time, and " +
			"a warning that cries wolf gets trained away")
	}
	if a, ok := byID["journal-pool"]; !ok || len(a) != 1 || a[0] != opKey {
		t.Errorf("the unsanctioned journal carve-out must be reported with its actor, got %v", byID["journal-pool"])
	}
	if a, ok := byID["mixed"]; !ok || len(a) != 1 || a[0] != thirdKey {
		t.Errorf("a MIXED carve-out must name only the uncovered actor (the remedy is per-actor), got %v", byID["mixed"])
	}
	if len(got) != 2 {
		t.Errorf("want exactly 2 risky carve-outs, got %d: %+v", len(got), got)
	}

	// And the reported deadline must be the carve-out's own, so an operator can schedule the remedy.
	for _, r := range got {
		if !r.ValidUntil.Equal(until) {
			t.Errorf("carve-out %q reported ValidUntil %v, want %v", r.CarveOutID, r.ValidUntil, until)
		}
	}

	// A fully-covered configuration reports NOTHING — the check must be able to come back clean, or it is
	// noise rather than a signal.
	clean := Config{Sanctioned: map[string][]string{"journal": {opKey}}, SelfActors: map[string]string{},
		CarveOuts: []CarveOut{{ID: "journal-pool", Domain: "journal", Actors: []string{opKey}, ValidUntil: until}}}
	if r := CarveOutExpiryRisk(clean); len(r) != 0 {
		t.Errorf("a fully-sanctioned configuration must report no risk, got %+v", r)
	}
}
