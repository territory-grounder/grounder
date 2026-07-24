package estate

import "sort"

// ShuffledControl builds a degree-preserving shuffled copy of the graph and returns the blast radius of
// target under it — the genuine negative control that makes the prediction gate FALSIFIABLE (INV-22). The
// port-fidelity audit flagged TG's prior control as fake: it sliced arbitrary hosts, preserving neither
// degree nor rel-type structure. This is the real thing, faithful to infragraph.py's shuffled_control:
//
//   - edges are bucketed by rel_type;
//   - within each bucket the TO endpoints are deterministically permuted (a seeded Fisher–Yates), so every
//     source keeps its exact out-degree and each rel_type keeps its multiset of target endpoints, while the
//     real who-depends-on-what topology is destroyed;
//   - the SAME depth-capped blast-radius walk then runs over the shuffled graph.
//
// If the real prediction is not materially more precise than this control, the graph encodes no real signal
// and the gate is judged to mean nothing. The shuffle is deterministic in seed, so the control is
// reproducible for a given day/plan.
//
// includeSiblings must mirror whatever the REAL prediction included: when the real prediction adds common-
// cause siblings (an availability/connectivity incident), the control walks the SHUFFLED graph's siblings too,
// so the two are the same shape and beating the control stays a genuine signal. Passing false when the real
// prediction added siblings (or true when it did not) rigs the comparison and is a correctness bug.
func (g *Graph) ShuffledControl(target Entity, maxDepth int, seed string, includeSiblings bool) []Impact {
	shuf := NewGraph(WithClock(g.now))

	// bucket edges by rel_type, collecting each bucket's ordered To endpoints
	type ekey struct{ from, to Entity }
	byRel := map[RelType][]*Edge{}
	for _, e := range g.edges {
		byRel[e.Rel] = append(byRel[e.Rel], e)
	}
	// deterministic bucket order
	rels := make([]RelType, 0, len(byRel))
	for r := range byRel {
		rels = append(rels, r)
	}
	sort.Slice(rels, func(i, j int) bool { return rels[i] < rels[j] })

	rng := newDetRand(seed)
	for _, r := range rels {
		edges := byRel[r]
		// stable order for reproducibility
		sort.Slice(edges, func(i, j int) bool {
			return edgeKey(edges[i].From, edges[i].To, r) < edgeKey(edges[j].From, edges[j].To, r)
		})
		targets := make([]Entity, len(edges))
		for i, e := range edges {
			targets[i] = e.To
		}
		// Fisher–Yates permute the target endpoints within the bucket
		for i := len(targets) - 1; i > 0; i-- {
			j := int(rng.next() % uint64(i+1))
			targets[i], targets[j] = targets[j], targets[i]
		}
		for i, e := range edges {
			shuf.Upsert(Edge{From: e.From, To: targets[i], Rel: r, Confidence: e.Confidence, Source: e.Source, ValidUntil: e.ValidUntil, ExpectedAlerts: e.ExpectedAlerts})
		}
	}
	imps := shuf.BlastRadius(target, maxDepth)
	if includeSiblings {
		imps = append(imps, shuf.Siblings(target)...)
	}
	return imps
}

// detRand is a tiny deterministic PRNG (splitmix64) seeded from a string — no global math/rand, replay-stable.
type detRand struct{ state uint64 }

func newDetRand(seed string) *detRand {
	var h uint64 = 1469598103934665603 // FNV-1a offset
	for i := 0; i < len(seed); i++ {
		h ^= uint64(seed[i])
		h *= 1099511628211
	}
	return &detRand{state: h}
}

func (r *detRand) next() uint64 {
	r.state += 0x9E3779B97F4A7C15
	z := r.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}
