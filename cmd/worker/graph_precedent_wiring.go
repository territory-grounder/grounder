package main

import (
	"strings"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/knowledge"
)

// estateBlastHosts adapts the estate causal graph into the knowledge plane's BlastHostsFunc seam (TG-53): for
// an alerting host it returns the names of the entities in its BLAST RADIUS — those that fail WITH it
// (who-depends-on-it), walked to depth. It reads holder.Graph() LIVE on each call so a graph refresh takes
// effect without a restart, and returns nil when the holder/graph is unavailable or nothing depends on the
// host — the pure-pass-through cases GraphExpandRetriever collapses to the base ranking. It is a named helper
// rather than an inline closure specifically so the estate↔knowledge join is unit-testable off a constructed
// graph, not only exercised live.
func estateBlastHosts(holder *estate.Holder, depth int) knowledge.BlastHostsFunc {
	return func(host string) []string {
		if holder == nil {
			return nil
		}
		g := holder.Graph()
		if g == nil {
			return nil
		}
		imps := g.BlastRadius(estate.Entity{Name: host}, depth)
		out := make([]string, 0, len(imps))
		for _, im := range imps {
			if n := strings.TrimSpace(im.Entity.Name); n != "" {
				out = append(out, n)
			}
		}
		return out
	}
}
