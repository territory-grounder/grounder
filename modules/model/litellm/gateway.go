// Package litellm is the loadable bundled LiteLLM model-gateway module (spec/008 REQ-815, T-008-19) and
// the family lead for the model-provider surface.
//
// It exposes ONE OpenAI-compatible endpoint fronting the configured provider backends, resolves
// component→model routing through one source of truth (adapters/model.Resolver), applies the configured
// auto-fallback ladder on a provider error or rate-limit, and records REAL-token usage per request read
// from the provider response — never fabricated. Every model response is returned as untrusted, typed data
// that the caller must never treat as control flow, a command string, or a query fragment (INV-08). The
// HTTP transport is injectable (a Doer) so the oracle drives routing/fallback/usage against a fake gateway.
// The gateway key is a secret reference, resolved per request, never a literal (INV-13).
//
// Provenance: [O] INV-08/INV-13, spec/008.
package litellm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	model "github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/config"
)

// SourceType is the vendor slug this module serves.
const SourceType = "litellm"

// Doer is the minimal HTTP contract; *http.Client satisfies it, and tests inject a fake gateway.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Usage is the real-token usage recorded per request, READ from the provider response — never fabricated.
type Usage struct {
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// Module is the LiteLLM gateway adapter. Construct with New.
type Module struct {
	baseURL  string
	keyRef   config.SecretRef
	resolver model.Resolver // the ONE component→model source of truth
	ladder   []string       // the auto-fallback ladder of model names (z.ai → DeepSeek → Mistral → …)
	http     Doer

	mu    sync.Mutex
	usage []Usage

	// Per-rung circuit breakers over a shared store: a provider that fails persistently is short-circuited
	// (skipped) rather than retried on every request — the resilience gap the predecessor's ladder had.
	brStore  breaker.Store
	brMu     sync.Mutex
	breakers map[string]*breaker.Breaker
}

// Option configures a Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP transport (a fake in tests, *http.Client in production).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// WithBreakerStore injects the shared circuit-breaker store (a pgx store in production for cross-process
// coordination; defaults to a per-module in-memory store so the breakers are always live, never a dead
// capability). Each fallback rung gets its own named breaker.
func WithBreakerStore(s breaker.Store) Option {
	return func(m *Module) {
		if s != nil {
			m.brStore = s
		}
	}
}

// New builds a LiteLLM gateway module for a base URL, a key secret reference, the component→model resolver
// (one source of truth), and the fallback ladder of model names.
func New(baseURL string, keyRef config.SecretRef, resolver model.Resolver, ladder []string, opts ...Option) *Module {
	m := &Module{
		baseURL: strings.TrimRight(baseURL, "/"), keyRef: keyRef, resolver: resolver, ladder: ladder,
		http: http.DefaultClient, brStore: breaker.NewMemStore(), breakers: map[string]*breaker.Breaker{},
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// breakerFor returns the per-rung circuit breaker for a model name, creating it lazily over the shared
// store. The name is sanitized to a stable metric-safe slug (e.g. "z.ai" → "model-z-ai").
func (m *Module) breakerFor(modelName string) *breaker.Breaker {
	m.brMu.Lock()
	defer m.brMu.Unlock()
	if b, ok := m.breakers[modelName]; ok {
		return b
	}
	b, err := breaker.New(modelBreakerName(modelName), m.brStore)
	if err != nil {
		b, _ = breaker.New("model-rung", m.brStore)
	}
	m.breakers[modelName] = b
	return b
}

func modelBreakerName(modelName string) string {
	var sb strings.Builder
	sb.WriteString("model-")
	for _, r := range modelName {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteRune('-')
		}
	}
	return sb.String()
}

// SourceType implements adapters/model registration identity.
func (m *Module) SourceType() string { return SourceType }

// Usage returns a copy of the recorded per-request token usage.
func (m *Module) Usage() []Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Usage, len(m.usage))
	copy(out, m.usage)
	return out
}

type chatRequest struct {
	Model    string          `json:"model"`
	Messages []model.Message `json:"messages"`
	User     string          `json:"user,omitempty"`
}

type usageBlock struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message model.Message `json:"message"`
	} `json:"choices"`
	Usage usageBlock `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// callModel posts one completion for a specific model name to the gateway. A transport error, a non-2xx
// status, or an error body is returned as an error (so the caller can advance the fallback ladder).
func (m *Module) callModel(ctx context.Context, user, modelName string, msgs []model.Message) (string, usageBlock, error) {
	key, err := m.keyRef.Resolve()
	if err != nil {
		return "", usageBlock{}, fmt.Errorf("litellm: resolve gateway key: %w", err)
	}
	body, err := json.Marshal(chatRequest{Model: modelName, Messages: msgs, User: user})
	if err != nil {
		return "", usageBlock{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", usageBlock{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := m.http.Do(req)
	if err != nil {
		return "", usageBlock{}, fmt.Errorf("litellm: gateway call: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", usageBlock{}, fmt.Errorf("litellm: %s status %d", modelName, resp.StatusCode)
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", usageBlock{}, fmt.Errorf("litellm: decode: %w", err)
	}
	if cr.Error != nil {
		return "", usageBlock{}, fmt.Errorf("litellm: %s error: %s", modelName, cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", usageBlock{}, fmt.Errorf("litellm: %s empty completion", modelName)
	}
	return cr.Choices[0].Message.Content, cr.Usage, nil
}
