package opclasscluster

// THE READY RESOLVER (TG-227 blocker 1). Until this existed the production Job carried Ready: nil —
// deliberately fail-closed, and terminally so: no candidate could EVER reach ratify_ready, which made
// ratification, the overlay, and the whole earned ladder unreachable in production. This supplies the
// three dossier facts the occurrence journal cannot know (REQ-2811):
//
//   - the never-auto screen, run over the candidate's slug/op AND every distinct observed op and target,
//     so a destructive command hiding under a benign slug ("restart-service" occurrences that actually
//     ran rm -rf) is caught by what was OBSERVED, not by what the slug claims;
//   - the mechanically assigned family/tier from the closed sets (ambiguity fails closed);
//   - estate blast-radius coverage: the fraction of distinct occurrence targets the estate graph can
//     resolve and walk. Unresolvable targets count AGAINST coverage — a dossier that cannot say what an
//     action would hit is not complete evidence, and zero targets is zero coverage, not vacuous 100%.

import (
	"context"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/opclasscat"
)

// blastRadiusDepth mirrors the walk depth the prediction path uses: enough to see a dependency fan-out,
// bounded so a dense graph cannot stall the cron.
const blastRadiusDepth = 4

// NewReadyResolver builds the production ReadyResolver over the live estate graph. The graph comes
// through a getter because the holder swaps it on refresh — the resolver must read the CURRENT graph
// each pass, not the boot-time one.
func NewReadyResolver(graph func() *estate.Graph, now func() time.Time) ReadyResolver {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return func(ctx context.Context, c opclasscat.Candidate, occs []opclasscat.Occurrence) (opclasscat.ReadyInput, error) {
		// THE SCREEN RUNS OVER WHAT WAS OBSERVED. Distinct ops and targets from the journal ride along
		// with the slug, so the verdict reflects the candidate's actual behaviour.
		seen := map[string]bool{}
		var observed []string
		var targets []string
		for _, o := range occs {
			for _, s := range []string{o.Op, o.Target} {
				s = strings.TrimSpace(s)
				if s == "" || seen[s] {
					continue
				}
				seen[s] = true
				observed = append(observed, s)
			}
			if t := strings.TrimSpace(o.Target); t != "" {
				targets = append(targets, t)
			}
		}
		barred := opclasscat.ScreenAutoBarred(c.OpClass, c.Op, observed...)

		family, famOK := opclasscat.AssignFamily(c.OpClass, c.Op)
		in := opclasscat.ReadyInput{
			AutoBarredStamped: true,
			AutoBarred:        barred,
			DismissActive:     c.DismissUntil != nil && c.DismissUntil.After(now()),
		}
		if famOK {
			in.Family = family
			in.Tier = opclasscat.AssignTier(family, barred)
		}

		// COVERAGE FAILS CLOSED. No graph, no targets, or unresolvable targets all hold the candidate
		// below the gate — never a manufactured 100%.
		g := graph()
		if g == nil || len(targets) == 0 {
			return in, nil // BlastRadiusCoverage stays 0
		}
		distinct := map[string]bool{}
		covered := 0
		for _, t := range targets {
			key := strings.ToLower(t)
			if distinct[key] {
				continue
			}
			distinct[key] = true
			if ent, ok := g.Resolve(t); ok {
				_ = g.BlastRadius(ent, blastRadiusDepth) // the walk must be COMPUTABLE; emptiness is honest
				covered++
			}
		}
		in.BlastRadiusCoverage = float64(covered) / float64(len(distinct))
		return in, nil
	}
}
