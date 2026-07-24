package litellm

import (
	"context"
	"fmt"

	model "github.com/territory-grounder/grounder/adapters/model"
)

// sequence returns the ordered list of model names to try for a component: the component's resolved model
// first (the one source of truth), then each ladder rung not already tried. This is the mechanical
// realization of "resolve through one router, then apply the configured auto-fallback ladder".
func (m *Module) sequence(component string) []string {
	seq := []string{}
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			seq = append(seq, name)
		}
	}
	add(m.resolver.Model(component))
	for _, rung := range m.ladder {
		add(rung)
	}
	return seq
}

// Complete routes a component to its model and serves the request over the one OpenAI-compatible endpoint.
// On a provider error or rate-limit it advances the configured auto-fallback ladder to the next rung. The
// real-token usage of the served request is read from the provider response and recorded (never
// fabricated). The returned text is untrusted, typed data — the caller must never treat it as control flow
// (INV-08). If every rung fails, the last error is returned and no usage is recorded.
func (m *Module) Complete(ctx context.Context, component, user string, msgs []model.Message) (string, Usage, error) {
	seq := m.sequence(component)
	if len(seq) == 0 {
		return "", Usage{}, fmt.Errorf("litellm: no model resolved for component %q and no fallback ladder", component)
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
	return "", Usage{}, fmt.Errorf("litellm: all %d fallback rungs failed for component %q: %w", len(seq), component, lastErr)
}
