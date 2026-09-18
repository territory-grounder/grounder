package estate

import "sort"

// InfraParentGroup groups host names by the single infrastructure parent they run on. A group whose Hosts
// has 2+ members is a SINGLE-POINT-OF-FAILURE concentration: one silent failure of Parent takes all of them
// at once. It is the standing risk that is knowable at boot rather than at the outage (TG-394: 7 of 26 of
// TG's own dependency hosts sat on the one node it was diagnosing, silently degrading retrieval for 11h12m).
type InfraParentGroup struct {
	Parent Entity   // the runs_on parent — a hypervisor / infrastructure node whose failure cascades
	Hosts  []string // the queried host names that run on Parent, de-duplicated and sorted; len >= 1
}

// InfraParentGroups groups the given host names by their best-confidence `runs_on` parent.
//
// ONLY runs_on TO A CASCADING INFRA NODE IS A COMMON CAUSE. A shared SITE is co-location, not co-failure; a
// shared SERVICE would itself alert, so it is not a SILENT common cause. member_of / depends_on / routes_via
// parents are excluded by the relation filter, and the parent TYPE is additionally checked against
// siblingParentEligible — the SAME allow-list the sibling walk uses — so a malformed `runs_on` edge into a
// non-cascading type (possible because the edge schema is observe-only, not enforced) cannot be read as a
// hypervisor concentration. Two hosts sharing a rack site is not the risk this measures; two sharing a
// hypervisor is.
//
// A host with no fresh runs_on parent in the graph is OMITTED, not treated as safe — its placement is
// unknown, and "unknown" must never read as "not concentrated". The caller compares the resolved host count
// (summed over the returned groups) against the number of names it passed and publishes that as coverage,
// so a partial resolution is legible rather than a silent understatement of the risk.
//
// Names are matched exactly as Parents matches them (canonical), and duplicate input names collapse to one.
// Groups are returned largest-first, then by parent name — deterministic, so a scrape is diff-stable.
func (g *Graph) InfraParentGroups(hostNames []string) []InfraParentGroup {
	byParent := map[string]*InfraParentGroup{}
	seen := map[string]bool{}
	for _, h := range hostNames {
		ch := canonName(h)
		if ch == "" || seen[ch] {
			continue
		}
		seen[ch] = true
		for _, p := range g.Parents(Entity{Name: h}) {
			if p.Rel != RelRunsOn || !siblingParentEligible(p.Entity.Type) {
				continue
			}
			// A host is placed on its SINGLE best-confidence runs_on parent. Parents is confidence-descending,
			// so the first runs_on edge is the authoritative placement; stop there rather than double-count a
			// host that also carries a stale/lower-confidence runs_on edge from another source.
			key := canonName(p.Entity.Name)
			grp := byParent[key]
			if grp == nil {
				grp = &InfraParentGroup{Parent: p.Entity}
				byParent[key] = grp
			}
			grp.Hosts = append(grp.Hosts, h)
			break
		}
	}
	out := make([]InfraParentGroup, 0, len(byParent))
	for _, grp := range byParent {
		sort.Strings(grp.Hosts)
		out = append(out, *grp)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Hosts) != len(out[j].Hosts) {
			return len(out[i].Hosts) > len(out[j].Hosts)
		}
		return out[i].Parent.Name < out[j].Parent.Name
	})
	return out
}
