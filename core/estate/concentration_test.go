package estate

import (
	"testing"
	"time"
)

// TG-394: the standing single-point-of-failure risk is knowable at BOOT, not at the outage. In the real
// incident 7 of 26 of TG's own dependency hosts sat on one hypervisor, and nothing reported the concentration
// until the node failed and retrieval silently went lexical-only for 11h12m. InfraParentGroups is the read
// that makes it visible: group dependency hosts by the hypervisor they run on; a parent carrying 2+ is a
// concentration.
//
// KILLING MUTATIONS (executed 2026-08-11):
//   - drop the `if p.Rel != RelRunsOn { continue }` filter: the member_of SITE edge below is counted as a
//     shared parent and TestConcentrationExcludesNonRunsOnParents goes RED — co-location read as co-failure.
//   - remove the `break` (count every runs_on edge): a host with two runs_on edges double-counts.
//   - assume an unresolved host is placed somewhere: TestUnresolvedHostsAreOmittedNotAssumedSafe goes RED.

func vmEnt(n string) Entity { return Entity{Type: TypeVM, Name: n} }

func TestInfraParentGroups_ConcentrationOnOneParent(t *testing.T) {
	now := time.Now()
	g := NewGraph(WithClock(func() time.Time { return now }))
	// 3 of 4 dependency hosts run on pve03; the 4th on pve04 — the exact TG-394 shape (the bulk on one node).
	g.Upsert(Edge{From: lxc("dep-a"), To: pveNode("pve03"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	g.Upsert(Edge{From: lxc("dep-b"), To: pveNode("pve03"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	g.Upsert(Edge{From: vmEnt("dep-c"), To: pveNode("pve03"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	g.Upsert(Edge{From: lxc("dep-d"), To: pveNode("pve04"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})

	groups := g.InfraParentGroups([]string{"dep-a", "dep-b", "dep-c", "dep-d"})
	if len(groups) != 2 {
		t.Fatalf("want 2 parents (pve03, pve04), got %d: %+v", len(groups), groups)
	}
	// Largest-first: pve03 with 3 hosts is THE concentration the ticket's killing mutation names.
	if groups[0].Parent.Name != "pve03" || len(groups[0].Hosts) != 3 {
		t.Errorf("want pve03 with 3 hosts (the single-point-of-failure concentration), got %s with %d: %v",
			groups[0].Parent.Name, len(groups[0].Hosts), groups[0].Hosts)
	}
	if groups[1].Parent.Name != "pve04" || len(groups[1].Hosts) != 1 {
		t.Errorf("want pve04 with 1 host, got %s with %d", groups[1].Parent.Name, len(groups[1].Hosts))
	}
}

func TestConcentrationExcludesNonRunsOnParents(t *testing.T) {
	now := time.Now()
	g := NewGraph(WithClock(func() time.Time { return now }))
	// Two hosts share only a SITE (member_of) and have NO runs_on placement. A shared site is co-location,
	// not co-failure. WITHOUT the runs_on filter the site is taken as their common parent and reported as a
	// 2-host concentration — the exact false positive this excludes; WITH it, both are omitted (unknown
	// placement) and nothing is reported. This is the assertion the RelRunsOn filter is load-bearing for.
	g.Upsert(Edge{From: lxc("dep-a"), To: Entity{Type: TypeSite, Name: "nl"}, Rel: RelMemberOf, Confidence: 0.90, Source: SourceNetbox})
	g.Upsert(Edge{From: lxc("dep-b"), To: Entity{Type: TypeSite, Name: "nl"}, Rel: RelMemberOf, Confidence: 0.90, Source: SourceNetbox})

	if groups := g.InfraParentGroups([]string{"dep-a", "dep-b"}); len(groups) != 0 {
		t.Errorf("two hosts sharing only a SITE (member_of, no runs_on) must NOT read as a concentration — "+
			"co-location is not a silent common cause, and a non-runs_on parent is not a hypervisor whose "+
			"failure cascades; got groups %+v", groups)
	}
}

func TestConcentrationExcludesMalformedNonInfraRunsOn(t *testing.T) {
	now := time.Now()
	g := NewGraph(WithClock(func() time.Time { return now }))
	// The edge schema is observe-only (not enforced), so a malformed adapter COULD emit a runs_on edge into a
	// non-cascading type. Two hosts "runs_on" the same SITE must NOT read as a hypervisor concentration —
	// siblingParentEligible excludes the site type even though the relation is runs_on.
	g.Upsert(Edge{From: lxc("dep-a"), To: Entity{Type: TypeSite, Name: "nl"}, Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	g.Upsert(Edge{From: lxc("dep-b"), To: Entity{Type: TypeSite, Name: "nl"}, Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})

	if groups := g.InfraParentGroups([]string{"dep-a", "dep-b"}); len(groups) != 0 {
		t.Errorf("a runs_on edge into a non-cascading TYPE (here a site) must be excluded by the "+
			"siblingParentEligible check, not read as a 2-host hypervisor concentration; got %+v", groups)
	}
}

func TestConcentrationIsPerHypervisorNotPerSite(t *testing.T) {
	now := time.Now()
	g := NewGraph(WithClock(func() time.Time { return now }))
	// Two hosts on DIFFERENT hypervisors but the same site: co-located, not co-failing. Each lands on its own
	// pve parent; neither parent carries 2, so there is no concentration.
	g.Upsert(Edge{From: lxc("dep-a"), To: pveNode("pve01"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	g.Upsert(Edge{From: lxc("dep-b"), To: pveNode("pve02"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	g.Upsert(Edge{From: lxc("dep-a"), To: Entity{Type: TypeSite, Name: "nl"}, Rel: RelMemberOf, Confidence: 0.90, Source: SourceNetbox})
	g.Upsert(Edge{From: lxc("dep-b"), To: Entity{Type: TypeSite, Name: "nl"}, Rel: RelMemberOf, Confidence: 0.90, Source: SourceNetbox})

	for _, grp := range g.InfraParentGroups([]string{"dep-a", "dep-b"}) {
		if len(grp.Hosts) >= 2 {
			t.Errorf("hosts on DIFFERENT hypervisors were grouped as a concentration: %s has %v", grp.Parent.Name, grp.Hosts)
		}
	}
}

func TestUnresolvedHostsAreOmittedNotAssumedSafe(t *testing.T) {
	now := time.Now()
	g := NewGraph(WithClock(func() time.Time { return now }))
	g.Upsert(Edge{From: lxc("dep-a"), To: pveNode("pve03"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	// dep-x has no runs_on edge — its placement is UNKNOWN, which is not the same as "not concentrated".
	groups := g.InfraParentGroups([]string{"dep-a", "dep-x", "dep-a"}) // dep-a duplicated → must collapse to one
	resolved := 0
	for _, grp := range groups {
		resolved += len(grp.Hosts)
	}
	if resolved != 1 {
		t.Errorf("only dep-a is placeable (dep-x unknown, dep-a de-duplicated); want resolved=1, got %d: %+v", resolved, groups)
	}
}

// TG-365 emptiness: nil hosts and unknown hosts over an empty graph yield no groups and never panic.
func TestInfraParentGroups_Empty(t *testing.T) {
	g := NewGraph()
	if got := g.InfraParentGroups(nil); len(got) != 0 {
		t.Errorf("nil hosts must yield no groups, got %v", got)
	}
	if got := g.InfraParentGroups([]string{"nope", ""}); len(got) != 0 {
		t.Errorf("unknown/empty host names over an empty graph must yield no groups, got %v", got)
	}
}
