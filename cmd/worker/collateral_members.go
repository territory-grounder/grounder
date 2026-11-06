package main

import "github.com/territory-grounder/grounder/core/estate"

// collateralBlastMembers enumerates the members the TG-483 terminus collateral re-check scans: the anchor and
// its blast radius (depth 3), over TOPOLOGY edges only.
//
// A path through a LEARNED co-occurrence edge (estate.SourceIncident) is excluded. Those edges record that
// two hosts alerted together, not that one depends on the other; core/estate says they "only ENRICH the
// blast-radius prediction" and may never suppress on their own. The collateral re-check BLOCKS graduation
// credit and auto-close, so a learned-only path was letting co-occurrence block a real heal. Seen in the
// TG-624 G3 soak (2026-11-06 12:17 NL): a restart-container on an NL guest lost its credit because a GR guest
// stopped by a SEPARATE soak fault was "collateral". The two hosts had co-alarmed in earlier drills, and the
// soak keeps teaching such edges. Nil graph or unresolvable host: nil (the activity reports UNKNOWN).
func collateralBlastMembers(g *estate.Graph, host string) []string {
	if g == nil {
		return nil
	}
	e, ok := g.Resolve(host)
	if !ok {
		return nil
	}
	out := []string{e.Name}
	for _, imp := range g.BlastRadius(e, 3) {
		if imp.Learned {
			continue
		}
		out = append(out, imp.Entity.Name)
	}
	return out
}
