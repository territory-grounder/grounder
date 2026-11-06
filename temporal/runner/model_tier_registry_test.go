package runner

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
)

// TestEverySelectedTierIsServedByTheGatewayConfig is the gate that replaced adapters/model.Resolver.
//
// TG-298 deleted a Go-side component→model map that was constructed once, in a test, and reached no
// composition root. The argument for deleting rather than wiring it is that the component→provider/model
// resolver of record ALREADY exists and is live: deploy/litellm-config.yaml's model_list. That argument is
// only worth anything if the two halves are held together — the Go side picks a tier NAME, and the config
// must actually serve a model under that name. Nothing checked that. A tier renamed on either side is
// invisible until production, where LiteLLM answers "Invalid model name" with HTTP 400 and every triage
// session on that tier proposes nothing.
//
// So this is the positive counterpart of the deletion: it makes the config load-bearing and checked,
// instead of a comment claiming it is the source of truth.
//
// SCOPE. It checks the tiers this package's selectors emit with no eval arm set, plus the judge's tier from
// the one rubric. It deliberately does NOT check the TG-204 arm overrides: armTier is unvalidated ON PURPOSE
// (activities.go) — a typo there must surface as a loud litellm 400 rather than silently serving the
// default, which is the failure mode that makes an A/B measure one arm twice.
//
// KILLING MUTATION: rename the returned tier in investigateTierFor from "fast" to "fast-tier" (or rename
// `model_name: fast` in deploy/litellm-config.yaml). This test goes RED with:
//
//	the Go side selects model tier "fast-tier" (investigateTierFor, ordinary incident) but
//	deploy/litellm-config.yaml serves no model under that name — every call on that tier gets HTTP 400
//	"Invalid model name" from the gateway and that caller silently produces nothing. Served: [...]
func TestEverySelectedTierIsServedByTheGatewayConfig(t *testing.T) {
	served := servedModelNames(t)

	// Collected by CALLING the live selectors, not from a literal list — a list would go stale the moment a
	// selector changed, which is the drift this gate exists to catch.
	selected := []struct{ tier, origin string }{
		{investigateTierFor(ingest.IncidentEnvelope{ExternalRef: "x1", Host: "h", AlertRule: "r", Severity: ingest.SeverityCritical}, ""), "investigateTierFor, critical incident (the MECH-402 floor)"},
		{investigateTierFor(ingest.IncidentEnvelope{ExternalRef: "x2", Host: "h", AlertRule: "r", Severity: ingest.SeverityWarning}, ""), "investigateTierFor, ordinary incident"},
		{decisionTierFor(), "decisionTierFor, the one forced-decision cycle"},
		{judge.DefaultParams().Model, "core/judge.DefaultParams().Model, from rubric.json"},
	}
	distinct := map[string]bool{}
	for _, s := range selected {
		distinct[s.tier] = true
	}

	// ★ VACUITY FLOOR. Both sides of this comparison are discovered, so both can silently collapse: a yaml
	// key rename empties `served`, and a selector returning "" (or every selector collapsing onto one tier)
	// empties the thing being checked. Either way every assertion below would pass while checking nothing.
	if len(served) < 2 {
		t.Fatalf("deploy/litellm-config.yaml yielded %d model_name entries — the parse is broken or the "+
			"key was renamed, and this gate would pass vacuously", len(served))
	}
	if len(distinct) < 2 {
		t.Fatalf("the production selectors produced %d DISTINCT model tier(s) (%v) — expected at least the "+
			"fast/primary split, so either the safety floor collapsed or this gate is checking one name twice",
			len(distinct), sortedKeys(distinct))
	}

	for _, s := range selected {
		if s.tier == "" {
			t.Fatalf("a production selector returned an EMPTY model tier (%s) — the gateway would receive a "+
				"request with no model and reject it", s.origin)
		}
		if !served[s.tier] {
			t.Errorf("the Go side selects model tier %q (%s) but deploy/litellm-config.yaml serves no model "+
				"under that name — every call on that tier gets HTTP 400 \"Invalid model name\" from the "+
				"gateway and that caller silently produces nothing. Served: %v",
				s.tier, s.origin, sortedKeys(served))
		}
	}

	// ★ NEGATIVE CONTROL. The check above is a map lookup; if `served` were ever built as a
	// match-everything set (a default-true map, a glob), it would accept any tier and this whole file would
	// be decoration. A name that must never be served proves the lookup can still say no.
	if served["tier-that-must-never-exist"] {
		t.Fatal("the served-model lookup accepted a fabricated tier name — it matches everything, so it " +
			"can never fail and proves nothing about the real tiers")
	}
}

// servedModelNames reads the model names deploy/litellm-config.yaml serves. It reads the REAL committed
// file rather than a fixture: a fixture would drift from the box, which is the whole defect being gated.
func servedModelNames(t *testing.T) map[string]bool {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", "litellm-config.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the gateway config that maps every tier to a provider (%s): %v", path, err)
	}
	var cfg struct {
		ModelList []struct {
			ModelName string `yaml:"model_name"`
		} `yaml:"model_list"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]bool{}
	for _, m := range cfg.ModelList {
		if m.ModelName != "" {
			out[m.ModelName] = true
		}
	}
	return out
}

// sortedKeys renders a set stably so a failure message is diffable between runs.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
