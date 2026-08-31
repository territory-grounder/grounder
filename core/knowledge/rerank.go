package knowledge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// rerank.go is the cross-encoder rerank stage of the advanced-RAG plane (TG-50): the lexical + RRF-fused base
// retrieves a WIDE recall set, then a cross-encoder rescoring reorders it to a precise top-k — the "maximize
// recall first, then rerank to precision" shape the source audits called for. The reranker is a proper
// cross-encoder (BAAI/bge-reranker-v2-m3 on the GPU aux node), which judges (incident ↔ precedent) relevance
// jointly rather than as two independent embeddings, so it promotes the precedent whose RESOLUTION actually
// fits this fault above one that merely shares symptom tokens. It ships OFF and degrades to the base ranking on
// any reranker failure, so it can never make retrieval worse than the shipped lexical/RRF cut.

// RerankScore is one candidate's cross-encoder relevance: its index in the input texts and a score where
// higher = more relevant.
type RerankScore struct {
	Index int
	Score float64
}

// Reranker rescores each candidate TEXT against the query with a cross-encoder. The production implementation
// is a HuggingFace Text-Embeddings-Inference /rerank client (adapters/rerank); tests inject a fake. It is a
// NETWORK call, so it returns an error the retriever DEGRADES on (falls back to the base order) — a reranker
// outage lowers quality but never fails a retrieval.
type Reranker interface {
	Rerank(ctx context.Context, query string, texts []string) ([]RerankScore, error)
}

// DefaultRerankWiden is how many candidates RerankRetriever pulls from the base before reranking to the
// caller's k — the wide recall set the cross-encoder promotes from. Bounded so one retrieval stays one bounded
// rerank call, not an unbounded sweep.
const DefaultRerankWiden = 20

// defaultRerankTimeout bounds the per-query rerank round trip; past it the query degrades to the base ranking
// (retrieval must never stall the investigation on a slow reranker).
const defaultRerankTimeout = 5 * time.Second

// RerankRetriever wraps a base Retriever with the cross-encoder rerank stage (TG-50): it pulls a WIDE candidate
// set from the base, rescoring them by (incident ↔ precedent) cross-encoder relevance, and returns the precise
// top-k. OFF by default — a nil Reranker (or a rerank error/timeout, or a base result of 0–1 hits) serves the
// base ranking EXACTLY, so unset is byte-identical and a reranker outage degrades to base, never fails.
type RerankRetriever struct {
	Base     Retriever
	Reranker Reranker
	WidenTo  int           // candidates pulled from Base before reranking; <=0 ⇒ DefaultRerankWiden
	Timeout  time.Duration // per-rerank budget; <=0 ⇒ defaultRerankTimeout
}

var _ Retriever = (*RerankRetriever)(nil)

// Retrieve returns up to k hits, reranked by the cross-encoder over a wide base candidate set. Every
// degradation path returns the base ranking unchanged.
func (r *RerankRetriever) Retrieve(q Query, k int) []Hit {
	if r.Base == nil || k <= 0 {
		return nil
	}
	if r.Reranker == nil {
		return r.Base.Retrieve(q, k) // OFF ⇒ base ranking, byte-identical
	}
	widen := r.WidenTo
	if widen <= 0 {
		widen = DefaultRerankWiden
	}
	if widen < k {
		widen = k
	}
	cands := r.Base.Retrieve(q, widen)
	if len(cands) <= 1 {
		return cands // nothing to reorder (already ≤ k)
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultRerankTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	texts := make([]string, len(cands))
	for i, h := range cands {
		texts[i] = rerankCandidateText(h.Incident)
	}
	scores, err := r.Reranker.Rerank(ctx, rerankQueryText(q), texts)
	if err != nil || len(scores) == 0 {
		return firstK(cands, k) // degrade to base ranking on any rerank failure
	}
	return firstK(reorderByRerank(cands, scores), k)
}

// Count delegates the novelty-gate signature count to the base — reranking does not change how many prior
// incidents share a (host, rule) signature (a corpus property), as the other wrapping retrievers do.
func (r *RerankRetriever) Count(host, alertRule string) int {
	if c, ok := r.Base.(interface {
		Count(string, string) int
	}); ok {
		return c.Count(host, alertRule)
	}
	return 0
}

// reorderByRerank returns cands reordered by the rerank scores (highest first), each reranked hit annotated
// with a "cross-encoder reranked" reason for explainability. It PRESERVES each Hit's original Score and Reasons
// (the rerank changes ORDER, not the lexical/RRF score scale the min-relevance floor and other stages read),
// and is defensive: a score whose index is out of range or duplicated is ignored, and any candidate the
// reranker did NOT score keeps its relative base order AFTER the scored ones — so a truncating or misbehaving
// reranker can never DROP a precedent, only fail to promote it.
func reorderByRerank(cands []Hit, scores []RerankScore) []Hit {
	scored := make([]bool, len(cands))
	valid := make([]RerankScore, 0, len(scores))
	for _, s := range scores {
		if s.Index >= 0 && s.Index < len(cands) && !scored[s.Index] {
			valid = append(valid, s)
			scored[s.Index] = true
		}
	}
	sort.SliceStable(valid, func(i, j int) bool { return valid[i].Score > valid[j].Score })
	out := make([]Hit, 0, len(cands))
	for _, s := range valid {
		h := cands[s.Index]
		h.Reasons = append(h.Reasons, fmt.Sprintf("cross-encoder reranked %.3f", s.Score))
		out = append(out, h)
	}
	for i, h := range cands {
		if !scored[i] {
			out = append(out, h)
		}
	}
	return out
}

func firstK(hits []Hit, k int) []Hit {
	if len(hits) > k {
		return hits[:k]
	}
	return hits
}

// rerankQueryText renders the incident as the rerank QUERY — the fault's identity in plain prose. No embed
// role-prefix (the cross-encoder is a different model than the asymmetric embedder): rule + host + summary is
// what a precedent is judged relevant TO.
func rerankQueryText(q Query) string {
	var b strings.Builder
	writeRerankField(&b, q.AlertRule)
	writeRerankField(&b, q.Host)
	writeRerankField(&b, q.Summary)
	return b.String()
}

// rerankCandidateText renders a precedent as a rerank CANDIDATE — its identity PLUS the resolution that worked,
// so the cross-encoder judges "does this prior FIX match this incident", not merely symptom overlap.
func rerankCandidateText(inc Incident) string {
	var b strings.Builder
	writeRerankField(&b, inc.AlertRule)
	writeRerankField(&b, inc.Host)
	writeRerankField(&b, inc.Summary)
	writeRerankField(&b, inc.Resolution)
	return b.String()
}

func writeRerankField(b *strings.Builder, v string) {
	if v = strings.TrimSpace(v); v != "" {
		if b.Len() > 0 {
			b.WriteString(". ")
		}
		b.WriteString(v)
	}
}
