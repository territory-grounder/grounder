package estate

import "testing"

// SiteOf is the estate-derived host→site vocabulary the verdict's coincidental-cross-site filter keys on
// (spec/002 REQ-107). The property under test throughout: the mapping answers ONLY from graph data (declared
// membership or a registered site's name prefix) and answers UNKNOWN for everything else — because the
// verdict treats unknown as never-excluded, a false "known" here is the fail-open direction.

func siteGraph() *Graph {
	g := NewGraph()
	// Declared membership: the firewall is a member_of its site (the operator's one explicit claim).
	g.Upsert(Edge{
		From: Entity{Type: TypeNetworkDevice, Name: "dc1fw01"},
		To:   Entity{Type: TypeSite, Name: "dc1"},
		Rel:  RelMemberOf, Source: SourceDeclared, Confidence: 0.85,
	})
	g.Upsert(Edge{
		From: Entity{Type: TypeNetworkDevice, Name: "dc2fw01"},
		To:   Entity{Type: TypeSite, Name: "dc2"},
		Rel:  RelMemberOf, Source: SourceDeclared, Confidence: 0.85,
	})
	// Ordinary topology: guests on hypervisors, no per-host site declarations.
	g.Upsert(Edge{
		From: Entity{Type: TypeLXC, Name: "dc1mealie01"},
		To:   Entity{Type: TypePVENode, Name: "dc1pve01"},
		Rel:  RelRunsOn, Source: SourcePVE, Confidence: 0.95,
	})
	g.Upsert(Edge{
		From: Entity{Type: TypeVM, Name: "dc2lte01"},
		To:   Entity{Type: TypePVENode, Name: "dc2pve01"},
		Rel:  RelRunsOn, Source: SourcePVE, Confidence: 0.95,
	})
	// A cross-site VPS with no site-prefixed name and no membership — must stay site-less (never excluded).
	g.Upsert(Edge{
		From: Entity{Type: TypeHost, Name: "notrf01vps01"},
		To:   Entity{Type: TypeTunnel, Name: "dc1fw01"},
		Rel:  RelRoutesVia, Source: SourceTunnel, Confidence: 1.0,
	})
	return g
}

func TestSiteOfDeclaredMembershipWins(t *testing.T) {
	g := siteGraph()
	if s, ok := g.SiteOf("dc1fw01"); !ok || s != "dc1" {
		t.Fatalf("member_of tier: SiteOf(dc1fw01) = (%q,%v), want (dc1,true)", s, ok)
	}
}

func TestSiteOfNamePrefixTierCoversUndeclaredHosts(t *testing.T) {
	g := siteGraph()
	// No member_of edge for the guests — the registered site names cover them by the naming convention.
	if s, ok := g.SiteOf("dc1mealie01"); !ok || s != "dc1" {
		t.Fatalf("prefix tier: SiteOf(dc1mealie01) = (%q,%v), want (dc1,true)", s, ok)
	}
	if s, ok := g.SiteOf("dc2lte01"); !ok || s != "dc2" {
		t.Fatalf("prefix tier: SiteOf(dc2lte01) = (%q,%v), want (dc2,true)", s, ok)
	}
	// Domain-qualified and case variants share the normalized identity, so they derive the same site.
	if s, ok := g.SiteOf("NLLEI01PVE01.mgmt.lan"); !ok || s != "dc1" {
		t.Fatalf("prefix tier (normalized): SiteOf(NLLEI01PVE01.mgmt.lan) = (%q,%v), want (dc1,true)", s, ok)
	}
}

func TestSiteOfUnknownHostStaysUnknown(t *testing.T) {
	g := siteGraph()
	// The VPS carries neither a membership nor a site-prefixed name: the estate makes no site claim, and the
	// verdict must therefore never cross-site-exclude it (a genuine tunnel cascade would be lost).
	if s, ok := g.SiteOf("notrf01vps01"); ok {
		t.Fatalf("SiteOf(notrf01vps01) = (%q,true), want unknown — an unclaimed host must stay site-less", s)
	}
	if _, ok := g.SiteOf(""); ok {
		t.Fatal("SiteOf(\"\") must be unknown")
	}
	if _, ok := (*Graph)(nil).SiteOf("dc1pve01"); ok {
		t.Fatal("a nil graph must answer unknown, never panic")
	}
}

func TestSiteOfEmptyEstateDerivesNothing(t *testing.T) {
	g := NewGraph()
	if _, ok := g.SiteOf("dc1pve01"); ok {
		t.Fatal("an estate with no seeded site entities must derive NO site for any host (config-not-code: " +
			"the vocabulary is graph data, and absent data must reproduce the never-exclude posture)")
	}
}

func TestSiteOfLongestPrefixWinsAndShortNamesNeverMatch(t *testing.T) {
	g := NewGraph()
	// Two nested site names: the more specific one must win for hosts under it.
	g.Upsert(Edge{
		From: Entity{Type: TypeNetworkDevice, Name: "dc1fw01"},
		To:   Entity{Type: TypeSite, Name: "nl"},
		Rel:  RelMemberOf, Source: SourceDeclared, Confidence: 0.85,
	})
	g.Upsert(Edge{
		From: Entity{Type: TypeNetworkDevice, Name: "dc1fw02"},
		To:   Entity{Type: TypeSite, Name: "dc1"},
		Rel:  RelMemberOf, Source: SourceDeclared, Confidence: 0.85,
	})
	if s, ok := g.SiteOf("dc1pve01"); !ok || s != "dc1" {
		t.Fatalf("longest prefix must win: got (%q,%v), want (dc1,true)", s, ok)
	}
	// A one-byte site name must never claim hosts by prefix (coincidence, not naming convention).
	g2 := NewGraph()
	g2.Upsert(Edge{
		From: Entity{Type: TypeHost, Name: "gateway"},
		To:   Entity{Type: TypeSite, Name: "n"},
		Rel:  RelMemberOf, Source: SourceDeclared, Confidence: 0.85,
	})
	if s, ok := g2.SiteOf("dc1pve01"); ok {
		t.Fatalf("a %d-byte site name must not prefix-claim hosts, got (%q,true)", 1, s)
	}
}

func TestSiteOfSiteNodeIsNotItsOwnHost(t *testing.T) {
	g := siteGraph()
	// Asking for the site entity itself: the prefix tier must not report a site as belonging to itself
	// (membership of a site in itself is not a host claim; only a real member_of edge could say otherwise).
	if s, ok := g.SiteOf("dc1"); ok {
		t.Fatalf("SiteOf(dc1) over the site node itself = (%q,true), want unknown", s)
	}
}
