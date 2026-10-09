package verify

import (
	"testing"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/safety"
)

// The scoped author (REQ-107/REQ-108) restores the predecessor's two verdict-scoping mechanics. These oracles
// drive the REAL authorities end to end — the SiteAuthority is a live estate.Graph.SiteOf over a seeded
// graph (never a stub map), and the family matching runs over the embedded production rulefamily.json — so a
// regression in either authority fails HERE, not only in its own package.

// scopedEstate builds the two-site estate the filter reasons over: sites dc1 and dc2 registered via
// one declared member_of edge each, so every dc1*/dc2* host derives its site by the naming tier,
// while notrf01vps01 (a tunnel-routed VPS) stays site-UNKNOWN.
func scopedEstate() *estate.Graph {
	g := estate.NewGraph()
	g.Upsert(estate.Edge{
		From: estate.Entity{Type: estate.TypeNetworkDevice, Name: "dc1fw01"},
		To:   estate.Entity{Type: estate.TypeSite, Name: "dc1"},
		Rel:  estate.RelMemberOf, Source: estate.SourceDeclared, Confidence: 0.85,
	})
	g.Upsert(estate.Edge{
		From: estate.Entity{Type: estate.TypeNetworkDevice, Name: "dc2fw01"},
		To:   estate.Entity{Type: estate.TypeSite, Name: "dc2"},
		Rel:  estate.RelMemberOf, Source: estate.SourceDeclared, Confidence: 0.85,
	})
	return g
}

func scopedPred() Prediction {
	return Prediction{
		ActionID: "a-scoped", PlanHash: "p-scoped", TargetHost: "dc1mealie01", Site: "dc1",
		PredictedHosts: map[string]struct{}{"dc1pve01": {}},
		PredictedRules: map[string]struct{}{RuleKey("dc1pve01", "HostDown"): {}},
	}
}

// REQ-107 (oracle iii): the exact live shape verdict.go:87 records — an unrelated sensor flap on the OTHER
// site during an dc1-targeted action. With BOTH sites estate-known and different it is excluded from the
// deviation evidence (match), while an unknown-site host in the same observation still deviates.
func TestScopedCrossSiteBothKnownDifferentIsExcluded(t *testing.T) {
	g := scopedEstate()
	obs := []ObservedAlert{
		{Host: "dc2lte01", Rule: "Sensor under limit - Check Device Health Settings", Site: "dc2"},
	}
	d := ComputeVerdictDetailScoped(scopedPred(), obs, nil, nil, g.SiteOf)
	if d.Verdict != safety.VerdictMatch {
		t.Fatalf("verdict = %q, want match — dc2lte01's site (dc2) and the target's (dc1) are BOTH "+
			"estate-known and DIFFER, so the flap is coincidental background, not this action's cascade; detail=%+v", d.Verdict, d)
	}
	if len(d.SurpriseHosts) != 0 || len(d.SurpriseAlerts) != 0 {
		t.Fatalf("excluded cross-site alert must leave NO deviation evidence, got %+v", d)
	}
}

func TestScopedUnknownSiteHostStillDeviates(t *testing.T) {
	g := scopedEstate()
	// The estate makes no site claim for the VPS (no membership, no site-prefixed name): the filter must NOT
	// exclude it — this is exactly the fail-closed direction (a genuine tunnel cascade would otherwise vanish).
	obs := []ObservedAlert{{Host: "notrf01vps01", Rule: "HostDown", Site: "whatever-the-ingest-stamped"}}
	d := ComputeVerdictDetailScoped(scopedPred(), obs, nil, nil, g.SiteOf)
	if d.Verdict != safety.VerdictDeviation {
		t.Fatalf("verdict = %q, want deviation — an unknown-site surprise host is NEVER cross-site-excluded", d.Verdict)
	}
	if len(d.SurpriseHosts) != 1 || d.SurpriseHosts[0] != "notrf01vps01" {
		t.Fatalf("surprise hosts = %v, want [notrf01vps01]", d.SurpriseHosts)
	}
}

func TestScopedSameSiteSurpriseStillDeviates(t *testing.T) {
	g := scopedEstate()
	// A surprise on the SAME site as the target is candidate cascade evidence — site scoping must not touch it.
	obs := []ObservedAlert{{Host: "dc1db01", Rule: "HighLatency", Site: "dc1"}}
	d := ComputeVerdictDetailScoped(scopedPred(), obs, nil, nil, g.SiteOf)
	if d.Verdict != safety.VerdictDeviation {
		t.Fatalf("verdict = %q, want deviation — a same-site surprise host is real deviation evidence", d.Verdict)
	}
}

func TestScopedUnknownTargetSiteExcludesNothing(t *testing.T) {
	g := scopedEstate()
	p := scopedPred()
	p.TargetHost = "unmapped-target01" // the estate cannot place the TARGET ⇒ there is no "other" site to prove
	obs := []ObservedAlert{{Host: "dc2lte01", Rule: "HostDown", Site: "dc2"}}
	d := ComputeVerdictDetailScoped(p, obs, nil, nil, g.SiteOf)
	if d.Verdict != safety.VerdictDeviation {
		t.Fatalf("verdict = %q, want deviation — with the target's site unknown the cross-site filter must be inert", d.Verdict)
	}
}

func TestScopedNilAuthorityReproducesWithBaselines(t *testing.T) {
	// The ingest-supplied Site label alone must never exclude: a nil authority is byte-identical to the
	// unscoped baselined author, which fails closed on every site label.
	obs := []ObservedAlert{{Host: "dc2lte01", Rule: "HostDown", Site: "dc2"}}
	scoped := ComputeVerdictDetailScoped(scopedPred(), obs, nil, nil, nil)
	plain := ComputeVerdictDetailWithBaselines(scopedPred(), obs, nil, nil)
	if scoped.Verdict != plain.Verdict || scoped.Verdict != safety.VerdictDeviation {
		t.Fatalf("nil-authority scoped=%q plain=%q, want both deviation (no vocabulary ⇒ nothing excluded)",
			scoped.Verdict, plain.Verdict)
	}
}

// REQ-108 (oracle iv): a predicted host firing a FAMILY SIBLING of its predicted rule — the same physical
// condition under another source's spelling, per the embedded production rulefamily.json — scores match, not
// partial and never deviation. A genuinely different-family rule on the same host still scores partial.
func TestScopedFamilySiblingRuleOnPredictedHostIsNotAMismatch(t *testing.T) {
	p := scopedPred() // predicts (dc1pve01, "HostDown"); "Devices-up/down" is its family sibling
	obs := []ObservedAlert{{Host: "dc1pve01", Rule: "Devices-up/down", Site: "dc1"}}
	d := ComputeVerdictDetailScoped(p, obs, nil, nil, nil)
	if d.Verdict != safety.VerdictMatch {
		t.Fatalf("verdict = %q, want match — Devices-up/down and HostDown share the device-down family "+
			"(core/knowledge rulefamily.json), so the predicted failure mode DID fire; detail=%+v", d.Verdict, d)
	}
	if len(d.Mismatches) != 0 {
		t.Fatalf("a family sibling must not record a mismatch, got %+v", d.Mismatches)
	}
	// The same spelling variance through the UNSCOPED authors too — family matching is a property of the one
	// author, not of the scoped entry point.
	if v := ComputeVerdict(p, obs); v != safety.VerdictMatch {
		t.Fatalf("ComputeVerdict = %q, want match (single author: every entry point shares the family mechanic)", v)
	}
}

func TestScopedDifferentFamilyRuleOnPredictedHostStaysPartial(t *testing.T) {
	p := scopedPred()
	obs := []ObservedAlert{{Host: "dc1pve01", Rule: "DiskFull", Site: "dc1"}}
	d := ComputeVerdictDetailScoped(p, obs, nil, nil, nil)
	if d.Verdict != safety.VerdictPartial {
		t.Fatalf("verdict = %q, want partial — DiskFull is NOT in HostDown's family, so the host was foreseen "+
			"but its failure mode was not", d.Verdict)
	}
	if len(d.Mismatches) != 1 || d.Mismatches[0] != (RuleMismatch{Host: "dc1pve01", Rule: "DiskFull"}) {
		t.Fatalf("mismatches = %+v, want the one (dc1pve01, DiskFull) pair", d.Mismatches)
	}
}

func TestScopedFamilyMatchingNeverRescuesASurpriseHost(t *testing.T) {
	// Family matching applies ONLY to predicted hosts: an UNPREDICTED host firing a family sibling of some
	// OTHER host's predicted rule is still a surprise (per-host families, never a global rule pool).
	p := scopedPred()
	obs := []ObservedAlert{{Host: "dc1db01", Rule: "Devices-up/down", Site: "dc1"}}
	d := ComputeVerdictDetailScoped(p, obs, nil, nil, nil)
	if d.Verdict != safety.VerdictDeviation {
		t.Fatalf("verdict = %q, want deviation — the family mechanic must never downgrade a surprise HOST", d.Verdict)
	}
}

// The scoping mechanics and the baselines COMPOSE: a cross-site flap, a pre-existing pair, a pre-anomalous
// host, and a family-sibling rule are each removed by their own filter, and the remaining same-site surprise
// still deviates — no filter masks another, and the composition cannot launder a genuine surprise.
func TestScopedFiltersComposeWithoutLaunderingARealSurprise(t *testing.T) {
	g := scopedEstate()
	p := scopedPred()
	obs := []ObservedAlert{
		{Host: "dc2lte01", Rule: "Sensor under limit", Site: "dc2"}, // cross-site, both known ⇒ excluded
		{Host: "dc1old01", Rule: "DiskFull", Site: "dc1"},           // pre-existing pair ⇒ excluded (TG-148)
		{Host: "dc1sick01", Rule: "NewRule", Site: "dc1"},           // pre-anomalous host ⇒ excluded (REQ-106)
		{Host: "dc1pve01", Rule: "Devices-up/down", Site: "dc1"},    // family sibling on the predicted host ⇒ predicted
		{Host: "dc1surprise01", Rule: "ServiceDown", Site: "dc1"},   // SAME-site genuine surprise ⇒ must survive
	}
	baseline := []ObservedAlert{{Host: "dc1old01", Rule: "DiskFull", Site: "dc1"}}
	pre := map[string]bool{"dc1sick01": true}
	d := ComputeVerdictDetailScoped(p, obs, baseline, pre, g.SiteOf)
	if d.Verdict != safety.VerdictDeviation {
		t.Fatalf("verdict = %q, want deviation — the genuine same-site surprise must survive every filter", d.Verdict)
	}
	if len(d.SurpriseHosts) != 1 || d.SurpriseHosts[0] != "dc1surprise01" {
		t.Fatalf("surprise hosts = %v, want exactly [dc1surprise01]", d.SurpriseHosts)
	}
	if len(d.Mismatches) != 0 {
		t.Fatalf("mismatches = %+v, want none", d.Mismatches)
	}
}
