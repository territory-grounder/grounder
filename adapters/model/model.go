// Package model is the client for the bundled LiteLLM model-gateway.
//
// Provenance: [corrections] native agent loop over a bundled LiteLLM gateway — NO Claude Code CLI ·
// [R] paradigm-rules 3, 6 (model-and-vendor-agnostic; local-first is a mode, not the mission),
// "centralized model routing", P0-6.
//
// TG never talks to a provider directly and never launches a coding-CLI subprocess. It calls ONE
// OpenAI-compatible endpoint (the LiteLLM gateway, in deploy/docker-compose.yml). The auto-fallback
// ladder (z.ai → DeepSeek → Mistral → …), retries, rate-limit handling, and org budgets/quotas live
// as LiteLLM config (deploy/litellm-config.yaml). The Go side only selects a model name; LiteLLM maps
// it to the ladder. Provider keys resolve through core/config secret references only (INV-13).
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

// Message is one OpenAI-style chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Resolver maps a component (e.g. "agent", "judge", "rag-synth") to a model name understood by the
// gateway. It is the single component→model source of truth; org policy overrides layer on top.
type Resolver struct {
	Components map[string]string // component → model name (e.g. "agent" → "primary-ladder")
	Default    string
}

// Model returns the model name for a component, falling back to Default.
func (r Resolver) Model(component string) string {
	if m, ok := r.Components[component]; ok {
		return m
	}
	return r.Default
}

// Gateway is a minimal client for the LiteLLM OpenAI-compatible endpoint.
type Gateway struct {
	BaseURL   string           // e.g. http://litellm:4000
	APIKeyRef config.SecretRef // e.g. env:LITELLM_MASTER_KEY
	HTTP      *http.Client
}

// NewGateway constructs a gateway client with a sane default timeout.
func NewGateway(baseURL string, keyRef config.SecretRef) *Gateway {
	return &Gateway{BaseURL: baseURL, APIKeyRef: keyRef, HTTP: &http.Client{Timeout: 120 * time.Second}}
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	User     string    `json:"user,omitempty"` // per-user/agent budget attribution at the gateway (org-global quota)
}

type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Complete performs a chat completion for a user/agent against a model name (which LiteLLM resolves
// down the fallback ladder). It returns the assistant text. Model output is returned as DATA — callers
// (the agent loop) must never treat it as control flow, a command, or a query fragment (INV-08).
func (g *Gateway) Complete(ctx context.Context, user, modelName string, msgs []Message) (string, error) {
	key, err := g.APIKeyRef.Resolve()
	if err != nil {
		return "", fmt.Errorf("model: resolve gateway key: %w", err)
	}
	body, err := json.Marshal(chatRequest{Model: modelName, Messages: msgs, User: user})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := g.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("model: gateway call: %w", err)
	}
	defer resp.Body.Close()

	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", fmt.Errorf("model: decode: %w", err)
	}
	if cr.Error != nil {
		return "", fmt.Errorf("model: gateway error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("model: empty completion")
	}
	return cr.Choices[0].Message.Content, nil
}
