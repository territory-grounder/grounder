package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// The embeddings client speaks the OpenAI-compatible /v1/embeddings surface LiteLLM proxies: one bound
// model name, batched inputs, vectors returned in INDEX order (the wire order is not trusted), the gateway
// key from its secret reference.
func TestGatewayEmbeddings(t *testing.T) {
	t.Setenv("TG_TEST_EMBED_KEY", "k-123")
	var gotPath, gotAuth string
	var gotReq embeddingsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		if len(gotReq.Input) == 1 {
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2]}]}`))
			return
		}
		// Deliberately out of wire order: index decides.
		_, _ = w.Write([]byte(`{"data":[{"index":1,"embedding":[0.3,0.4]},{"index":0,"embedding":[0.1,0.2]}]}`))
	}))
	defer srv.Close()

	g := NewGateway(srv.URL, config.SecretRef("env:TG_TEST_EMBED_KEY"))
	vecs, err := g.Embeddings(context.Background(), "nomic-embed-text", []string{"a", "b"})
	if err != nil {
		t.Fatalf("embeddings: %v", err)
	}
	if gotPath != "/v1/embeddings" || gotAuth != "Bearer k-123" {
		t.Fatalf("wrong wire call: path=%s auth=%q", gotPath, gotAuth)
	}
	if gotReq.Model != "nomic-embed-text" || len(gotReq.Input) != 2 {
		t.Fatalf("wrong request body: %+v", gotReq)
	}
	if len(vecs) != 2 || vecs[0][0] != 0.1 || vecs[1][0] != 0.3 {
		t.Fatalf("vectors must return in index order: %+v", vecs)
	}

	// The bound Embedder is the knowledge plane's seam; unconfigured it refuses rather than fabricates.
	e := Embedder{Gateway: g, Model: "nomic-embed-text"}
	if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("bound embedder: %v", err)
	}
	if _, err := (Embedder{}).Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("an unconfigured embedder must refuse")
	}
	// Empty input needs no round trip and returns no vectors.
	if vecs, err := g.Embeddings(context.Background(), "nomic-embed-text", nil); err != nil || vecs != nil {
		t.Fatalf("empty input: %v %v", vecs, err)
	}
}

// Failure surfaces are errors, never fabricated vectors: a non-2xx status, a gateway error body, and a
// count mismatch between inputs and returned vectors.
func TestGatewayEmbeddingsFailures(t *testing.T) {
	t.Setenv("TG_TEST_EMBED_KEY", "k-123")
	for name, handler := range map[string]http.HandlerFunc{
		"non-2xx":        func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(502) },
		"error body":     func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"error":{"message":"no embed model"}}`)) },
		"count mismatch": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1]}]}`)) },
		"empty vector":   func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[]},{"index":1,"embedding":[0.1]}]}`)) },
	} {
		srv := httptest.NewServer(handler)
		g := NewGateway(srv.URL, config.SecretRef("env:TG_TEST_EMBED_KEY"))
		if _, err := g.Embeddings(context.Background(), "m", []string{"a", "b"}); err == nil {
			t.Errorf("%s: must surface as an error", name)
		}
		srv.Close()
	}
	// No model name is a config error, refused before any call.
	g := NewGateway("http://unreachable.invalid", config.SecretRef("env:TG_TEST_EMBED_KEY"))
	if _, err := g.Embeddings(context.Background(), "", []string{"a"}); err == nil {
		t.Fatal("a blank model name must be refused")
	}
}
