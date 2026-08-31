package main

import (
	"os"
	"strings"
	"testing"
)

// Phase-1b characterization test (TG-501): pin that buildModelGateway wires ALL THREE operator-set output
// bounds, so the god-file carve that extracted it from main() cannot silently drop one. MaxTokens is a public
// field (asserted behaviourally); the concurrency + session-budget caps land in unexported fields with no
// getter, so a source-scan pins that their setter calls survived the move — the composition-root idiom (read
// the source as text, assert the wiring), the same reasoning worker_wiring_inventory_test.go relies on.

func TestBuildModelGatewayAppliesMaxTokens(t *testing.T) {
	t.Setenv("TG_MODEL_MAX_TOKENS", "4096")
	gw := buildModelGateway()
	if gw == nil {
		t.Fatal("buildModelGateway returned nil")
	}
	if gw.MaxTokens != 4096 {
		t.Errorf("TG_MODEL_MAX_TOKENS=4096 not applied to the gateway: gw.MaxTokens=%d", gw.MaxTokens)
	}
}

func TestBuildModelGatewayWiresAllThreeOutputBounds(t *testing.T) {
	src, err := os.ReadFile("worker_model_budget.go")
	if err != nil {
		t.Fatalf("read worker_model_budget.go: %v", err)
	}
	s := string(src)
	// The concurrency + session-token caps set unexported fields (no getter), so pin their SETTER calls by
	// source: a call dropped in the carve would silently un-bound that lane with nothing behavioural to catch it.
	for _, want := range []string{
		`gw.SetMaxConcurrency(envInt("TG_MODEL_MAX_CONCURRENCY"`,
		`gw.MaxTokens = envInt("TG_MODEL_MAX_TOKENS"`,
		`gw.SetSessionTokenBudget(envInt("TG_MODEL_SESSION_TOKEN_BUDGET"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("buildModelGateway no longer wires %q — a model output-bound was dropped in the carve", want)
		}
	}
	// main() must actually CALL the extracted constructor — a carve that leaves it uncalled is a dead brain.
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(mainSrc), "buildModelGateway()") {
		t.Error("main.go no longer calls buildModelGateway() — the extracted gateway construction is unreferenced")
	}
}
