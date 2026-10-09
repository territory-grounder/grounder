// Package rerank is the cross-encoder rerank client for TG's advanced-RAG plane (TG-50): a HuggingFace
// Text-Embeddings-Inference (TEI) /rerank client that scores (query, candidate) pairs with a real cross-encoder
// (BAAI/bge-reranker-v2-m3 on the GPU auxiliary node — embeddings/rerank/small services only, never brain
// inference). It implements core/knowledge.Reranker, so RerankRetriever depends on the INTERFACE and never on
// this transport. The endpoint URL is injected from configuration (never a committed estate hostname); an
// unset URL leaves reranking OFF at the composition root.
package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/territory-grounder/grounder/core/knowledge"
)

// DefaultTimeout bounds one /rerank round trip when the client is given no HTTP client of its own.
const DefaultTimeout = 8 * time.Second

// TEIClient POSTs to a TEI /rerank endpoint. It carries NO API key: the reranker runs on the private aux node
// with no auth, and putting a key here would be a literal-secret smell for a service that has none. A non-2xx
// status or any transport/decoding error is returned as an error, which RerankRetriever degrades on (falls
// back to the base ranking) — a reranker outage lowers retrieval quality, never fails a retrieval.
type TEIClient struct {
	BaseURL string       // e.g. injected TG_RERANK_URL; the estate host is deploy config, never committed
	HTTP    *http.Client // nil ⇒ a default client bounded by DefaultTimeout
}

var _ knowledge.Reranker = (*TEIClient)(nil)

type teiRerankRequest struct {
	Query string   `json:"query"`
	Texts []string `json:"texts"`
}

// teiRerankResult is one element of TEI's /rerank response: the candidate's index in the request `texts` and
// its cross-encoder relevance score. TEI returns them sorted by score descending, but RerankRetriever re-sorts
// defensively rather than trusting the wire order.
type teiRerankResult struct {
	Index int     `json:"index"`
	Score float64 `json:"score"`
}

// Rerank calls TEI POST {BaseURL}/rerank with {query, texts} and returns the per-candidate scores. It never
// mutates argument slices and treats the response as untrusted numeric DATA (indices are range-checked by the
// caller). An empty query or texts is sent as-is; TEI handles the trivial case.
func (c *TEIClient) Rerank(ctx context.Context, query string, texts []string) ([]knowledge.RerankScore, error) {
	if c == nil || c.BaseURL == "" {
		return nil, fmt.Errorf("rerank: no endpoint configured")
	}
	body, err := json.Marshal(teiRerankRequest{Query: query, Texts: texts})
	if err != nil {
		return nil, fmt.Errorf("rerank: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("rerank: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rerank: POST %s/rerank: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("rerank: %s/rerank returned HTTP %d", c.BaseURL, resp.StatusCode)
	}
	var results []teiRerankResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, fmt.Errorf("rerank: decode response: %w", err)
	}
	out := make([]knowledge.RerankScore, 0, len(results))
	for _, r := range results {
		out = append(out, knowledge.RerankScore{Index: r.Index, Score: r.Score})
	}
	return out, nil
}
