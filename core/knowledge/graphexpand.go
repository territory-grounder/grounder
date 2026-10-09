package knowledge

import "strings"

// graphexpand.go is the incident-knowledge GraphRAG stage (TG-53): it links the precedent corpus to the
// estate causal graph so that "what past incidents hit the hosts in THIS alert's blast radius" becomes part
// of retrieval. For an incident on host H it retrieves precedent for H and for the hosts in H's blast radius
// (the entities that fail WITH H — who-depends-on-H), then Reciprocal-Rank-Fuses the lists, so a precedent on
// a topologically-coupled host is LIFTED above an equally-lexically-similar precedent on an unrelated host.
// It exploits an asset TG uniquely has (a first-class entity-relation estate graph beside the precedent
// corpus), which is why the two planes being disjoint made this the highest-value GraphRAG query unbuildable.

// BlastHostsFunc returns the estate blast-radius host names for an alerting host — the entities that fail with
// it (who-depends-on-it), as produced by core/estate's Graph.BlastRadius. It is injected as a plain func so
// the knowledge plane stays free of a core/estate dependency (and testable without a graph). It returns
// nil/empty when the graph is unavailable, the host is unknown, or nothing depends on it — every such case
// makes GraphExpandRetriever a pure pass-through to the base.
type BlastHostsFunc func(host string) []string

// DefaultGraphExpandHosts bounds how many blast-radius hosts are folded into one retrieval — the fan-out cap
// (each host is a full base retrieval), so a high-in-degree hub cannot turn one query into an unbounded sweep.
const DefaultGraphExpandHosts = 4

// GraphExpandRetriever wraps a base Retriever with ESTATE-GRAPH host broadening (TG-53). Deterministic (the
// graph walk + RRF are fixed given the graph state), and OFF by default: the composition root wraps the base
// only when armed (TG_RETRIEVE_GRAPH_PRECEDENT) AND a graph is available, so unset — or a nil BlastHosts, or a
// host with an empty blast radius — serves the base retriever's ranking EXACTLY.
type GraphExpandRetriever struct {
	Base       Retriever
	BlastHosts BlastHostsFunc
	MaxHosts   int // cap on blast-radius hosts folded in; <=0 ⇒ DefaultGraphExpandHosts
}

var _ Retriever = (*GraphExpandRetriever)(nil)

// Retrieve fuses the base retriever's results for the alerting host with its results for each blast-radius
// host variant (same rule/site/summary, a different host). With no host to expand, a nil graph func, or an
// empty blast radius it reduces to the base ranking exactly — the fused score of a single list is monotonic
// in rank, so a single-list "fusion" is a no-op.
func (r *GraphExpandRetriever) Retrieve(q Query, k int) []Hit {
	if r.Base == nil || k <= 0 {
		return nil
	}
	if r.BlastHosts == nil || strings.TrimSpace(q.Host) == "" {
		return r.Base.Retrieve(q, k)
	}
	hosts := r.BlastHosts(q.Host)
	if len(hosts) == 0 {
		return r.Base.Retrieve(q, k)
	}
	max := r.MaxHosts
	if max <= 0 {
		max = DefaultGraphExpandHosts
	}
	// The alerting host's list first; then one host-broadened variant per blast-radius host, bounded and
	// deduped (a variant whose host duplicates the alerting host or an earlier variant is skipped — the graph
	// can never inflate one host's weight by naming it twice).
	lists := [][]Hit{r.Base.Retrieve(q, k)}
	seen := map[string]bool{strings.ToLower(strings.TrimSpace(q.Host)): true}
	for _, h := range hosts {
		if len(lists) >= max+1 {
			break
		}
		hn := strings.ToLower(strings.TrimSpace(h))
		if hn == "" || seen[hn] {
			continue
		}
		seen[hn] = true
		v := q // shallow copy; Tags is read-only downstream (as in queryVariants)
		v.Host = h
		lists = append(lists, r.Base.Retrieve(v, k))
	}
	if len(lists) == 1 {
		return lists[0] // every blast-radius host deduped/emptied away ⇒ base ranking
	}
	return rrfMergeHits(lists, k)
}

// Count delegates the novelty-gate signature count to the base — host broadening does not change how many
// prior incidents share a (host, rule) signature (a corpus property), exactly as MultiQueryRetriever does.
func (r *GraphExpandRetriever) Count(host, alertRule string) int {
	if c, ok := r.Base.(interface {
		Count(string, string) int
	}); ok {
		return c.Count(host, alertRule)
	}
	return 0
}
