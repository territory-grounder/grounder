package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// Embeddings calls the gateway's OpenAI-compatible POST /v1/embeddings for a model name (LiteLLM proxies
// it to the configured embedding backend — an Ollama nomic-embed-text, an API embedding model, …; the
// model is config-not-code, TG_EMBED_MODEL). It returns exactly one vector per input text, in input order.
// Vectors are untrusted numeric DATA (INV-08): compared, never executed or interpolated. The gateway key
// resolves per request from its secret reference (INV-13), never a literal.
func (g *Gateway) Embeddings(ctx context.Context, modelName string, texts []string) ([][]float32, error) {
	if modelName == "" {
		return nil, fmt.Errorf("model: embeddings: no model name")
	}
	if len(texts) == 0 {
		return nil, nil // no gateway call is made, so nothing is observed
	}
	start := time.Now()
	// The RAG embedding backend gets the SAME named, persisted breaker as the completion tiers (TG-221 /
	// audit finding #24, which called out that RAG's lexical fallback was bounded per-call but never by a
	// named breaker — the predecessor's rag_embed_ollama). A trip returns a typed error, so the knowledge
	// plane degrades to its deterministic lexical path VISIBLY (outcome="breaker_open" in tg_model_calls_total)
	// instead of paying a dead round trip on every retrieval.
	br := g.breakerFor(modelName)
	if !g.Breakers.allow(ctx, br) {
		err := errBreakerOpen(br.Name(), modelName)
		g.observe("embed", "", ClassBreakerOpen, 0, time.Since(start).Seconds(), err.Error())
		return nil, err
	}
	out, status, outcome, err := g.doEmbed(ctx, modelName, texts)
	g.Breakers.record(ctx, br, outcome, err)
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	// The embedding plane shares the observability seam under the fixed tier label "embed" (bounded), so
	// a slow or failing embedding backend is visible in the SAME tg_model_* family as completions.
	g.observe("embed", "", outcome, status, time.Since(start).Seconds(), detail)
	return out, err
}

// doEmbed performs the embeddings call and returns the classified (vectors, status, outcome, error), mirroring
// Complete.do: failures return a typed *ModelError with the same bounded outcome classes.
func (g *Gateway) doEmbed(ctx context.Context, modelName string, texts []string) ([][]float32, int, string, error) {
	key, err := g.APIKeyRef.Resolve()
	if err != nil {
		return nil, 0, "transport", &ModelError{Class: "transport", Message: "resolve gateway key: " + err.Error(), wrapped: err}
	}
	body, err := json.Marshal(embeddingsRequest{Model: modelName, Input: texts})
	if err != nil {
		return nil, 0, "transport", &ModelError{Class: "transport", Message: err.Error(), wrapped: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, 0, "transport", &ModelError{Class: "transport", Message: err.Error(), wrapped: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := g.HTTP.Do(req)
	if err != nil {
		cls := classifyTransport(err)
		return nil, 0, cls, &ModelError{Class: cls, Message: "embeddings call: " + err.Error(), wrapped: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cls := classifyStatus(resp.StatusCode)
		return nil, resp.StatusCode, cls, &ModelError{Status: resp.StatusCode, Class: cls, Message: fmt.Sprintf("embeddings: gateway status %d", resp.StatusCode)}
	}
	var er embeddingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, resp.StatusCode, "provider_error", &ModelError{Status: resp.StatusCode, Class: "provider_error", Message: "embeddings decode: " + err.Error()}
	}
	if er.Error != nil {
		cls := classifyStatus(resp.StatusCode)
		return nil, resp.StatusCode, cls, &ModelError{Status: resp.StatusCode, Class: cls, Message: "embeddings: " + er.Error.Message}
	}
	if len(er.Data) != len(texts) {
		return nil, resp.StatusCode, "empty", &ModelError{Status: resp.StatusCode, Class: "empty", Message: fmt.Sprintf("got %d vectors for %d inputs", len(er.Data), len(texts))}
	}
	// The index field is authoritative for ordering (the wire order is not guaranteed).
	sort.Slice(er.Data, func(i, j int) bool { return er.Data[i].Index < er.Data[j].Index })
	out := make([][]float32, len(er.Data))
	for i, d := range er.Data {
		if len(d.Embedding) == 0 {
			return nil, resp.StatusCode, "empty", &ModelError{Status: resp.StatusCode, Class: "empty", Message: fmt.Sprintf("empty vector at index %d", d.Index)}
		}
		out[i] = d.Embedding
	}
	return out, resp.StatusCode, "ok", nil
}

type embeddingsRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embeddingsResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Embedder binds the gateway to ONE configured embedding model — the knowledge plane's
// Embed(ctx, texts) seam (core/knowledge.Embedder). The zero value is unusable by construction: an
// unconfigured embedder errors rather than silently fabricating vectors.
type Embedder struct {
	Gateway *Gateway
	Model   string // the embedding model name the gateway serves (TG_EMBED_MODEL)
}

// Embed produces one vector per text via the gateway.
func (e Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if e.Gateway == nil || e.Model == "" {
		return nil, fmt.Errorf("model: embedder not configured (gateway/model missing)")
	}
	return e.Gateway.Embeddings(ctx, e.Model, texts)
}
