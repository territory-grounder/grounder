package knowledge

import (
	"sort"
	"strings"
)

// MultiQueryRetriever wraps a base Retriever with deterministic MULTI-QUERY retrieval (TG-50, advanced-RAG):
// it retrieves over a small set of RULE-BROADENED query variants (the original + a host-relaxed one) and
// RECIPROCAL-RANK-FUSES them, so a relevant precedent the exact-host query ranks low — a same-fault-class
// incident on a DIFFERENT host — is surfaced by the broadened variant and lifted in the fused cut. It is
// DETERMINISTIC (no model in the path, no query rewrite via the LLM): the variants are a fixed relaxation of
// the query fields. Off by default: the composition root wraps the base retriever only when armed
// (TG_RETRIEVE_MULTIQUERY), so unset serves the base retriever's ranking exactly.
type MultiQueryRetriever struct {
	Base Retriever
}

var _ Retriever = (*MultiQueryRetriever)(nil)

// Retrieve fuses the base retriever's results over the query variants. With a single variant (nothing to
// relax) it reduces to the base ranking exactly — the fused score of a single list is monotonic in rank.
func (m *MultiQueryRetriever) Retrieve(q Query, k int) []Hit {
	if m.Base == nil || k <= 0 {
		return nil
	}
	variants := queryVariants(q)
	if len(variants) <= 1 {
		return m.Base.Retrieve(q, k)
	}
	lists := make([][]Hit, 0, len(variants))
	for _, v := range variants {
		lists = append(lists, m.Base.Retrieve(v, k))
	}
	return rrfMergeHits(lists, k)
}

// Count delegates the novelty-gate signature count to the base (query BROADENING does not change how many
// prior incidents share a signature — that is a corpus property). A base that does not expose Count yields 0,
// exactly as an un-wrapped retriever without the method would.
func (m *MultiQueryRetriever) Count(host, alertRule string) int {
	if c, ok := m.Base.(interface {
		Count(string, string) int
	}); ok {
		return c.Count(host, alertRule)
	}
	return 0
}

// queryVariants returns the deterministic set of queries to retrieve and fuse. The original is always first;
// a host-relaxed variant (same rule/site/tags/summary, no host) broadens to the same fault class on ANY host,
// surfacing cross-host precedent the exact-host query buries. A query with no host to relax yields just the
// original (so multi-query is a no-op there and Retrieve reduces to the base).
func queryVariants(q Query) []Query {
	out := []Query{q}
	if strings.TrimSpace(q.Host) != "" {
		hostRelaxed := q // shallow copy; Tags is read-only downstream
		hostRelaxed.Host = ""
		out = append(out, hostRelaxed)
	}
	return out
}

// rrfMergeHits fuses N ranked hit lists by Reciprocal Rank Fusion, using the SAME rrfK=60 constant fuseRRF
// uses: each list contributes 1/(rrfK + rank) per document (rank starting at 1), summed across lists, so a
// precedent surfaced by several variants outranks a single-variant one at comparable ranks. Ties break
// deterministically by ExternalRef (matching the lexical + fused cuts). Reasons are unioned. Returns top-k.
func rrfMergeHits(lists [][]Hit, k int) []Hit {
	type acc struct {
		inc     Incident
		score   float64
		reasons []string
		seen    map[string]bool
	}
	docs := map[string]*acc{}
	order := make([]string, 0)
	for _, list := range lists {
		for rank, h := range list {
			ref := h.Incident.ExternalRef
			d := docs[ref]
			if d == nil {
				d = &acc{inc: h.Incident, seen: map[string]bool{}}
				docs[ref] = d
				order = append(order, ref)
			}
			d.score += 1.0 / float64(rrfK+rank+1)
			for _, r := range h.Reasons {
				if !d.seen[r] {
					d.seen[r] = true
					d.reasons = append(d.reasons, r)
				}
			}
		}
	}
	hits := make([]Hit, 0, len(docs))
	for _, ref := range order {
		d := docs[ref]
		hits = append(hits, Hit{Incident: d.inc, Score: round4(d.score), Reasons: d.reasons})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Incident.ExternalRef < hits[j].Incident.ExternalRef
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}
