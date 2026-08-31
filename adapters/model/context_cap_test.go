package model

// TG-42 — the context-carried per-class output cap, on the WIRE. The seam exists so the runner can
// budget a cheap execution class below the class-blind TG_MODEL_MAX_TOKENS ceiling without a Completer
// signature change; its one safety property is TIGHTEN-ONLY — a context value can lower the effective
// max_tokens, never raise or disarm the ceiling an operator set. Killing mutation: replace
// effectiveMaxTokens' `n < m` with `n > m` (or drop the ctx read) — the tighten/never-raise assertions
// go red.

import (
	"context"
	"encoding/json"
	"testing"
)

func wireMaxTokens(t *testing.T, gatewayCap int, ctx context.Context) int {
	t.Helper()
	var body []byte
	srv := captureBody(t, &body)
	defer srv.Close()
	g := testGateway(t, srv.URL, nil)
	g.MaxTokens = gatewayCap
	if _, err := g.Complete(ctx, "u", "fast", []Message{{Role: "user", Content: "x"}}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	var got chatRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	return got.MaxTokens
}

func TestContextOutputCapTightensTheWireAndNeverRaisesIt(t *testing.T) {
	bg := context.Background()
	cases := []struct {
		name       string
		gatewayCap int
		ctx        context.Context
		want       int
	}{
		{"a class cap TIGHTENS an armed ceiling", 4096, WithOutputTokenCap(bg, 1500), 1500},
		{"a class cap can NEVER raise the ceiling", 4096, WithOutputTokenCap(bg, 9999), 4096},
		{"a class cap arms alone when no ceiling is set", 0, WithOutputTokenCap(bg, 1500), 1500},
		{"no class cap: the ceiling stands untouched", 4096, bg, 4096},
		{"a non-positive class cap stores nothing (inert)", 4096, WithOutputTokenCap(bg, 0), 4096},
		{"neither: max_tokens omitted (model default, today's behavior)", 0, bg, 0},
	}
	for _, tc := range cases {
		if got := wireMaxTokens(t, tc.gatewayCap, tc.ctx); got != tc.want {
			t.Errorf("%s: wire max_tokens = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestOutputTokenCapContextRoundTrip pins the seam the runner's oracles read through: a positive cap
// round-trips; an absent or non-positive one reads as absent (never a phantom zero-cap).
func TestOutputTokenCapContextRoundTrip(t *testing.T) {
	if n, ok := OutputTokenCapFromContext(context.Background()); ok || n != 0 {
		t.Fatalf("an unstamped context must carry no cap, got (%d,%v)", n, ok)
	}
	if n, ok := OutputTokenCapFromContext(WithOutputTokenCap(context.Background(), 1500)); !ok || n != 1500 {
		t.Fatalf("a stamped cap must round-trip, got (%d,%v)", n, ok)
	}
	if n, ok := OutputTokenCapFromContext(WithOutputTokenCap(context.Background(), -1)); ok || n != 0 {
		t.Fatalf("a non-positive cap must store nothing, got (%d,%v)", n, ok)
	}
}
