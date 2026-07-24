package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
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
		return nil, nil
	}
	key, err := g.APIKeyRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("model: resolve gateway key: %w", err)
	}
	body, err := json.Marshal(embeddingsRequest{Model: modelName, Input: texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("model: embeddings call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("model: embeddings: gateway status %d", resp.StatusCode)
	}
	var er embeddingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, fmt.Errorf("model: embeddings decode: %w", err)
	}
	if er.Error != nil {
		return nil, fmt.Errorf("model: embeddings: gateway error: %s", er.Error.Message)
	}
	if len(er.Data) != len(texts) {
		return nil, fmt.Errorf("model: embeddings: got %d vectors for %d inputs", len(er.Data), len(texts))
	}
	// The index field is authoritative for ordering (the wire order is not guaranteed).
	sort.Slice(er.Data, func(i, j int) bool { return er.Data[i].Index < er.Data[j].Index })
	out := make([][]float32, len(er.Data))
	for i, d := range er.Data {
		if len(d.Embedding) == 0 {
			return nil, fmt.Errorf("model: embeddings: empty vector at index %d", d.Index)
		}
		out[i] = d.Embedding
	}
	return out, nil
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
