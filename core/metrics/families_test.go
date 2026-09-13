package metrics

import (
	"strings"
	"testing"
)

// TestClampsBoundLabelCardinality proves every label value is folded to a CLOSED enum — the guarantee that
// a hostname/ref/op can never become an unbounded label on the /metrics surface.
func TestClampsBoundLabelCardinality(t *testing.T) {
	for in, want := range map[string]string{
		"proposed": "proposed", "stop": "stop", "escalate": "escalate", "hard-halt": "hard-halt",
		"": "other", "web01": "other", "anything-else": "other",
	} {
		if got := ClampAgentOutcome(in); got != want {
			t.Fatalf("ClampAgentOutcome(%q)=%q want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"match": "match", "partial": "partial", "deviation": "deviation",
		"": "unset", "unset": "unset", "bogus": "other",
	} {
		if got := ClampVerdict(in); got != want {
			t.Fatalf("ClampVerdict(%q)=%q want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"AUTO": "AUTO", "AUTO_NOTICE": "AUTO_NOTICE", "POLL_PAUSE": "POLL_PAUSE",
		"": "other", "dc1web01": "other",
	} {
		if got := ClampBand(in); got != want {
			t.Fatalf("ClampBand(%q)=%q want %q", in, got, want)
		}
	}
}

// TestFamilyConstructorsPresetNameKindHelp proves the constructors preset the metric identity and clamp
// their label values, so a caller cannot mislabel a family or smuggle an unbounded label through them.
func TestFamilyConstructorsPresetNameKindHelp(t *testing.T) {
	runs := AgentRunsSample("not-an-outcome", 3)
	if runs.Name != MetricAgentRuns || runs.Kind != Counter || runs.Labels["outcome"] != "other" {
		t.Fatalf("AgentRunsSample mislabeled: %+v", runs)
	}
	dec := DecisionsSample("not-a-band", true, 1)
	if dec.Labels["band"] != "other" || dec.Labels["withheld"] != "true" {
		t.Fatalf("DecisionsSample mislabeled: %+v", dec)
	}
	// the four base counters carry no label at all.
	for _, s := range []Sample{AgentRunSecondsSample(1), AgentToolCallsSample(1), AgentToolErrorsSample(1), AgentTokensApproxSample(1)} {
		if len(s.Labels) != 0 || s.Kind != Counter {
			t.Fatalf("base counter must be an unlabelled counter: %+v", s)
		}
	}

	// rendered together they stay grouped + deterministic (the exposition contract).
	out := Render([]Sample{
		AgentRunsSample("proposed", 1),
		VerdictsSample("match", 1),
		DecisionsSample("POLL_PAUSE", true, 1),
	})
	if strings.Count(out, "# TYPE tg_governance_decisions_total counter") != 1 {
		t.Fatalf("decision family must carry exactly one TYPE header:\n%s", out)
	}
}

// TestModelCallFamily proves the model-call family clamps its tier + outcome labels to bounded enums and
// renders the two metrics with the right names/labels — the observability surface for gateway calls.
func TestModelCallFamily(t *testing.T) {
	for in, want := range map[string]string{"primary": "primary", "fast": "fast", "embed": "embed", "moonshot/kimi-k3": "other", "": "other"} {
		if got := ClampModelTier(in); got != want {
			t.Fatalf("ClampModelTier(%q)=%q want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"ok": "ok", "empty": "empty", "rate_limit": "rate_limit", "timeout": "timeout",
		"bad_request": "bad_request", "auth": "auth", "provider_error": "provider_error", "transport": "transport",
		"": "other", "weird": "other",
	} {
		if got := ClampModelOutcome(in); got != want {
			t.Fatalf("ClampModelOutcome(%q)=%q want %q", in, got, want)
		}
	}

	c := ModelCallsSample("moonshot/kimi-k3", "429ish", 3)
	if c.Name != MetricModelCalls || c.Kind != Counter || c.Labels["model"] != "other" || c.Labels["outcome"] != "other" {
		t.Fatalf("ModelCallsSample mislabeled/unclamped: %+v", c)
	}
	s := ModelCallSecondsSample("primary", 42)
	if s.Name != MetricModelCallSeconds || s.Kind != Counter || s.Labels["model"] != "primary" || len(s.Labels) != 1 {
		t.Fatalf("ModelCallSecondsSample wrong: %+v", s)
	}
	out := Render([]Sample{ModelCallsSample("primary", "ok", 2), ModelCallSecondsSample("primary", 42)})
	if strings.Count(out, "# TYPE tg_model_calls_total counter") != 1 || !strings.Contains(out, `tg_model_calls_total{model="primary",outcome="ok"} 2`) {
		t.Fatalf("model-call exposition wrong:\n%s", out)
	}
}

// EVERY TIER TG CAN SELECT MUST SURVIVE THE LABEL CLAMP (TG-303).
//
// modelTierSet allowed exactly {primary, fast, embed}, while the Go side selects arm-haiku, arm-opus,
// opus-cc and judge — so all of them collapsed into "other". The TG-204 experiment arms are among them,
// which meant the A/B could not distinguish its own arms in the exposition layer: two arms, one series.
// A metric that cannot separate the thing being measured is not instrumentation, it is a flat line.
func TestEveryTierTheGatewayServesSurvivesTheClamp(t *testing.T) {
	// These are the model_name aliases in deploy/litellm-config.yaml — the set TG can actually ask for.
	for _, tier := range []string{
		"primary", "fast", "opus-cc", "arm-haiku", "arm-opus", "judge",
		"fallback-deepseek", "fallback-mistral", "fallback-zai", "embed-nomic",
	} {
		if got := ClampModelTier(tier); got != tier {
			t.Errorf("ClampModelTier(%q) = %q — this tier's metrics are indistinguishable from every other "+
				"unlisted tier, so anything comparing tiers (the TG-204 A/B most of all) is comparing a "+
				"merged series against itself", tier, got)
		}
	}
}

// The set must stay CLOSED. Its purpose is to bound label cardinality — a model name taken straight from a
// gateway response is config- or attacker-influenced — so widening it must not become passing input through.
func TestAnUnknownTierIsStillClampedRatherThanPassedThrough(t *testing.T) {
	for _, junk := range []string{"gpt-9-turbo", "", "primary; DROP", strings.Repeat("x", 300)} {
		if got := ClampModelTier(junk); got != "other" {
			t.Errorf("ClampModelTier(%q) = %q, want \"other\" — an unlisted tier must be clamped, or the "+
				"label set is unbounded and a single bad response can explode the metric cardinality", junk, got)
		}
	}
}
