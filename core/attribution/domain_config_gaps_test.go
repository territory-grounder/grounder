package attribution

import (
	"testing"
	"time"
)

// AN ARMED READER WITH NO IDENTITY DECLARED FOR ITS DOMAIN MAXIMISES ESCALATION, SILENTLY.
//
// Two distinct absences, both driven by a missing config row rather than by anything the running system says:
//
//   - no sanctioned principals for the domain -> every actor in it is AttributedSuspicious by construction
//   - no self-actor for the domain -> TG's OWN actions in it are AttributedSuspicious, i.e. TG raises a
//     security escalation on itself, and suspicion DOMINATES so that reading masks every other candidate
//
// SelfActors is populated for exactly one domain in the whole tree ("pve", from the actuation credential), so
// arming any other reader walks straight into the second case.
// allActing treats every domain as one TG actuates in — the pre-2026-07-29 behaviour these cases were
// written against. The NEW rule (a self-actor is only missing where TG ACTS) has its own oracles below.
var allActing = map[string]bool{"pve": true, "journal": true, "awx": true, "netbox": true, "ldapident": true, "gitopsmr": true}

func TestDomainConfigGapsNamesEachArmedReaderMissingAnIdentity(t *testing.T) {
	cfg := Config{
		SelfActors: map[string]string{"pve": "root@pam!tg-actuate"},
		Sanctioned: map[string][]string{"pve": {"root@pam"}},
	}

	gaps := DomainConfigGaps(cfg, []string{"pve", "journal", "awx"}, allActing)

	byDomain := map[string]DomainConfigGap{}
	for _, g := range gaps {
		byDomain[g.Domain] = g
	}

	// pve is fully declared — it must NOT be reported. Without this the function could return every armed
	// domain unconditionally and still satisfy the assertions below, which is the vacuous-control shape.
	if _, reported := byDomain["pve"]; reported {
		t.Errorf("pve has BOTH a self-actor and sanctioned principals declared and was still reported as a "+
			"gap: %+v — a warning that fires on a correct config trains operators to ignore the line",
			byDomain["pve"])
	}

	for _, d := range []string{"journal", "awx"} {
		g, ok := byDomain[d]
		if !ok {
			t.Errorf("domain %q is armed with NO self-actor and NO sanctioned principals and was not "+
				"reported — TG's own actions there read as attributed-suspicious and nothing says so", d)
			continue
		}
		if !g.NoSelfActor {
			t.Errorf("domain %q: NoSelfActor is false, but SelfActors has no entry for it. This is the case "+
				"where TG raises a SECURITY escalation on ITSELF", d)
		}
		if !g.NoSanctioned {
			t.Errorf("domain %q: NoSanctioned is false, but Sanctioned has no entry for it", d)
		}
	}
}

// The two absences are INDEPENDENT and must be reported independently — an operator who declared sanctioned
// principals but forgot the self identity has a different (and worse) problem than one who did neither, and
// collapsing them into a single boolean would hide it.
func TestTheTwoAbsencesAreReportedSeparately(t *testing.T) {
	cfg := Config{
		SelfActors: map[string]string{"journal": "tg-actuate"},    // self declared, sanctioned missing
		Sanctioned: map[string][]string{"netbox": {"svc-netbox"}}, // sanctioned declared, self missing
	}
	gaps := DomainConfigGaps(cfg, []string{"journal", "netbox"}, allActing)
	got := map[string]DomainConfigGap{}
	for _, g := range gaps {
		got[g.Domain] = g
	}

	j, ok := got["journal"]
	if !ok {
		t.Fatal("journal has a self-actor but NO sanctioned principals — every human actor there reads as " +
			"suspicious, so it is still a gap")
	}
	if j.NoSelfActor || !j.NoSanctioned {
		t.Errorf("journal reported %+v — want NoSelfActor=false, NoSanctioned=true", j)
	}

	n, ok := got["netbox"]
	if !ok {
		t.Fatal("netbox has sanctioned principals but NO self-actor — TG's own actions there read as suspicious")
	}
	if !n.NoSelfActor || n.NoSanctioned {
		t.Errorf("netbox reported %+v — want NoSelfActor=true, NoSanctioned=false", n)
	}
}

// A whitespace-only self-actor is NOT a declaration. An env var that resolved to "" or " " is the realistic
// way this ends up half-set, and treating it as present would silence the warning in exactly that case.
func TestABlankSelfActorIsNotADeclaration(t *testing.T) {
	cfg := Config{SelfActors: map[string]string{"pve": "   "}, Sanctioned: map[string][]string{"pve": {"root@pam"}}}
	gaps := DomainConfigGaps(cfg, []string{"pve"}, allActing)
	if len(gaps) != 1 || !gaps[0].NoSelfActor {
		t.Errorf("a whitespace-only self-actor was accepted as declared: %+v — a credential ref that "+
			"resolves empty is precisely how this goes wrong in production", gaps)
	}
	// No armed readers means no gaps — an unconfigured install must not emit a warning per domain it is not using.
	if g := DomainConfigGaps(Config{}, nil, allActing); len(g) != 0 {
		t.Errorf("with NO readers armed, %d gap(s) were reported: %+v", len(g), g)
	}
	if g := DomainConfigGaps(Config{}, []string{"", "  "}, allActing); len(g) != 0 {
		t.Errorf("blank domain entries were reported as gaps: %+v", g)
	}
}

// The gap report must agree with what Attribute() actually DOES, or it is decoration. This drives the real
// derivation: an armed domain with no sanctioned list resolves a plain human actor to attributed-suspicious.
func TestTheReportedGapMatchesTheDerivationItWarnsAbout(t *testing.T) {
	now := nowFunc()
	cfg := Config{
		SelfActors: map[string]string{"pve": "root@pam!tg-actuate"},
		Sanctioned: map[string][]string{}, // nothing declared for "journal"
		Window:     time.Hour,
		// Attribute() reads cfg.now(), which falls back to the REAL clock when Now is nil. The package's
		// nowFunc is a FIXED epoch, so leaving this unset put the evidence years outside the admissibility
		// window and the case resolved `unattributable` — the assertion below caught that, which is the point
		// of asserting the derivation rather than trusting the description of it.
		Now: nowFunc,
	}
	gaps := DomainConfigGaps(cfg, []string{"journal"}, allActing)
	if len(gaps) != 1 || !gaps[0].NoSanctioned {
		t.Fatalf("expected journal reported as missing sanctioned principals, got %+v", gaps)
	}

	ev := []Evidence{{
		Domain: "journal", Actor: "alice", ActionKind: "systemctl-restart", Target: "web01",
		ObservedAt: now.Add(-5 * time.Minute), Ref: "journal:1", Covered: true,
	}}
	f := Attribute("web01", "restart-service", ev, nil, cfg)
	if f.Taxonomy != AttributedSuspicious {
		t.Errorf("a human actor in a domain with NO sanctioned list resolved to %v, want attributed-suspicious. "+
			"If this changed, the boot warning now describes a consequence that no longer happens", f.Taxonomy)
	}
}

// ★ A SELF-ACTOR IS ONLY MISSING WHERE TG ACTS.
//
// A self-identity exists to stop TG reading its OWN action as a stranger's. In a domain TG only READS, there
// is no TG action in the evidence to misread — so demanding one sends an operator to wire something that
// cannot change any outcome.
//
// MEASURED 2026-07-29: `netbox` has NO write path anywhere in the tree (its reader declares ReadOnly() and
// nothing posts/puts/patches/deletes), so TG can never appear in a NetBox changelog. `awx` is the opposite —
// opschema.json declares effect_kind "awx-launch", so TG's own runs land in AWX job history and a missing
// self-identity there is real.
//
// This is the SECOND correction to this diagnostic today. The first (!706) fixed it naming a remedy that does
// not exist. This one fixes it demanding the remedy where it is not needed. A warning is not free: each false
// one spends an operator's attention and buys distrust of the next.
func TestAReadOnlyDomainIsNotMissingASelfActor(t *testing.T) {
	cfg := Config{SelfActors: map[string]string{}, Sanctioned: map[string][]string{
		"netbox": {"someone"}, "awx": {"someone"},
	}}
	acts := map[string]bool{"awx": true} // netbox is read-only

	gaps := DomainConfigGaps(cfg, []string{"netbox", "awx"}, acts)

	byDomain := map[string]DomainConfigGap{}
	for _, g := range gaps {
		byDomain[g.Domain] = g
	}
	if _, reported := byDomain["netbox"]; reported {
		t.Errorf("netbox was reported as a gap, but TG only READS it — there is no TG action in a NetBox "+
			"changelog for a self-identity to match, so the operator would wire something with no effect. "+
			"got: %+v", gaps)
	}
	g, ok := byDomain["awx"]
	if !ok || !g.NoSelfActor {
		t.Errorf("awx was NOT reported as missing a self-actor, but TG launches AWX jobs (effect_kind "+
			"awx-launch) and its own runs land in AWX job history — this is the case that is real, and "+
			"over-reporting netbox is what dilutes it. got: %+v", gaps)
	}
}

// The SANCTIONED half is unaffected by whether TG acts: a read-only domain still needs to classify the
// HUMANS it observes, or every one of them reads attributed-suspicious.
func TestAReadOnlyDomainStillNeedsSanctionedPrincipals(t *testing.T) {
	cfg := Config{SelfActors: map[string]string{}, Sanctioned: map[string][]string{}}
	gaps := DomainConfigGaps(cfg, []string{"netbox"}, map[string]bool{}) // TG acts nowhere

	if len(gaps) != 1 {
		t.Fatalf("a read-only domain with no sanctioned principals must still be reported, got %+v", gaps)
	}
	if gaps[0].NoSelfActor {
		t.Error("a read-only domain was reported as missing a SELF-actor — it has no TG action to attribute")
	}
	if !gaps[0].NoSanctioned {
		t.Error("the sanctioned half must still fire: without it every human the reader observes resolves " +
			"attributed-suspicious, which is a real and separate defect")
	}
}

// A domain TG acts in, WITH an identity wired, is clean on the self half — the direction that would
// otherwise nag forever after the wiring is done.
func TestAnActingDomainWithAnIdentityIsClean(t *testing.T) {
	cfg := Config{
		SelfActors: map[string]string{"journal": "root!SHA256:abc"},
		Sanctioned: map[string][]string{"journal": {"kp"}},
	}
	if gaps := DomainConfigGaps(cfg, []string{"journal"}, map[string]bool{"journal": true}); len(gaps) != 0 {
		t.Errorf("a fully-wired acting domain was still reported: %+v", gaps)
	}
}
