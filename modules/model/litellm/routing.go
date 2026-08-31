package litellm

import (
	"context"
	"fmt"

	model "github.com/territory-grounder/grounder/adapters/model"
)

// sequence returns the ordered list of model names to try: the caller's selected model first, then each
// ladder rung not already tried. The caller's name comes from the tier constant at its call site and is
// mapped to a provider by deploy/litellm-config.yaml; this function does NOT re-map it (TG-298 — the Go
// copy of that table was dead, and two tables for one decision is not "one source of truth").
func (m *Module) sequence(modelName string) []string {
	seq := []string{}
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			seq = append(seq, name)
		}
	}
	add(modelName)
	for _, rung := range m.ladder {
		add(rung)
	}
	return seq
}

// Complete serves a caller's selected model name over the one OpenAI-compatible endpoint. On a provider
// error or rate-limit it advances the configured auto-fallback ladder to the next rung. The real-token
// usage of the served request is read from the provider response and recorded (never fabricated). The
// returned text is untrusted, typed data — the caller must never treat it as control flow (INV-08). If
// every rung fails, the last error is returned and no usage is recorded.
//
// The argument order mirrors adapters/model.Gateway.Complete (ctx, user, modelName, msgs) — the client the
// worker actually builds — so the two model paths cannot be confused for two different contracts.
func (m *Module) Complete(ctx context.Context, user, modelName string, msgs []model.Message) (string, Usage, error) {
	seq := m.sequence(modelName)
	if len(seq) == 0 {
		return "", Usage{}, fmt.Errorf("litellm: no model name given (%q) and no fallback ladder", modelName)
	}
	var lastErr error
	for _, name := range seq {
		// A persistently-failing rung is short-circuited by its breaker (skipped) rather than retried on
		// every request — a provider outage no longer costs a failed round-trip per call. A degraded breaker
		// store fails open (allow), so losing breaker persistence never blocks the gateway.
		br := m.breakerFor(name)
		if allowed, _ := br.Allow(ctx); !allowed {
			lastErr = fmt.Errorf("litellm: %s circuit open — rung short-circuited", name)
			continue
		}
		text, ub, err := m.callModel(ctx, user, name, msgs)
		if err != nil {
			_ = br.RecordFailure(ctx)
			lastErr = err
			continue // advance the fallback ladder
		}
		_ = br.RecordSuccess(ctx)
		u := Usage{Model: name, PromptTokens: ub.PromptTokens, CompletionTokens: ub.CompletionTokens, TotalTokens: ub.TotalTokens}
		m.mu.Lock()
		m.usage = append(m.usage, u)
		m.mu.Unlock()
		return text, u, nil
	}
	return "", Usage{}, fmt.Errorf("litellm: all %d fallback rungs failed for model %q: %w", len(seq), modelName, lastErr)
}
