package knowledge

import (
	"context"
	"strings"
)

// QueryRewriter reformulates an incident's retrieval Query into a cleaner, retrieval-optimized one — e.g.
// normalizing vendor/shorthand terminology, promoting the salient symptom, dropping ticket noise — so a
// raw, noisily-worded alert retrieves as the query an operator would have typed. It returns the query
// UNCHANGED (or empty) to skip. This is a MODEL CALL, injected as a func so core/knowledge stays free of the
// model gateway (mirrors FusedRetriever.Hypothetical / the RerankRetriever's Reranker seam).
type QueryRewriter func(ctx context.Context, q Query) Query

// QueryRewriteRetriever (TG-50, the advanced-RAG query-rewrite stage) reformulates the query with an LLM
// BEFORE handing it to Base, then serves Base's hits for the rewritten query. It wraps OUTERMOST — the
// rewrite happens once and the entire fused + multi-query + graph-expand + rerank stack then runs on the
// improved query. OFF (nil Rewrite) ⇒ passthrough, byte-identical to the base ranking. It DEGRADES to the
// original query whenever the rewrite is empty, unchanged, or the rewriter fails (a rewrite must never fail
// a retrieval), so an LLM outage silently reverts to today's behavior.
type QueryRewriteRetriever struct {
	Base    Retriever
	Rewrite QueryRewriter // nil ⇒ passthrough
}

var _ Retriever = (*QueryRewriteRetriever)(nil)

func (r *QueryRewriteRetriever) Retrieve(q Query, k int) []Hit {
	if r.Rewrite == nil {
		return r.Base.Retrieve(q, k)
	}
	rq := r.Rewrite(context.Background(), q)
	// No useful rewrite ⇒ retrieve on the original. Two no-ops: (1) UNCHANGED — QueryText equal (the rewriter's
	// "skip" signal), and (2) CONTENT-LESS — nothing beyond the embedding prefix (a degenerate rewrite), so we
	// never retrieve on an empty query even if a rewriter misbehaves.
	if QueryText(rq) == QueryText(q) || strings.TrimSpace(strings.TrimPrefix(QueryText(rq), embedQueryPrefix)) == "" {
		return r.Base.Retrieve(q, k)
	}
	return r.Base.Retrieve(rq, k)
}
