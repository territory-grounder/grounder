package runner

// TG-42 — the per-class OUTPUT budget: the fast execution classes carry a tighter max_tokens cap
// (TG_MODEL_MAX_TOKENS_FAST) on every completion of their session; the deep classes keep the
// class-blind TG_MODEL_MAX_TOKENS plumbing byte-identically (current behavior is the default for the
// class that matters most). The gateway-side tighten-only arithmetic is oracled in adapters/model
// (context_cap_test.go); THESE oracles pin the class→cap selection and that the cap actually rides
// the session context into every model call.

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/ingest"
)

// TestFastClassOutputCapSelection pins the class→cap map over the CLOSED class enumeration: only the
// two fast classes read the env; deep/standard/human-led take 0 (no per-class cap — the class-blind
// ceiling stands untouched) even when the env is set; and a blank/zero/malformed/negative env is inert
// for everyone (the TG-48 shipping convention — a typo tightens nothing and widens nothing).
func TestFastClassOutputCapSelection(t *testing.T) {
	t.Setenv("TG_MODEL_MAX_TOKENS_FAST", "1500")
	for class, want := range map[execclass.Class]int{
		execclass.Deterministic:     1500,
		execclass.FastAgent:         1500,
		execclass.StandardAgent:     0,
		execclass.DeepInvestigation: 0,
		execclass.HumanLed:          0,
	} {
		if got := fastClassOutputCap(class); got != want {
			t.Errorf("class %s: cap = %d, want %d", class, got, want)
		}
	}
	for _, raw := range []string{"", "   ", "0", "abc", "-5"} {
		t.Setenv("TG_MODEL_MAX_TOKENS_FAST", raw)
		if got := fastClassOutputCap(execclass.FastAgent); got != 0 {
			t.Errorf("env %q must be inert (0), got %d — a malformed knob must leave the existing ceiling standing", raw, got)
		}
	}
}

// capProbe is a scripted model that records the context-carried output cap seen by EVERY completion.
type capProbe struct {
	responses []string
	i         int
	caps      []int
}

func (m *capProbe) Complete(ctx context.Context, _, _ string, _ []model.Message) (string, error) {
	n, _ := model.OutputTokenCapFromContext(ctx)
	m.caps = append(m.caps, n)
	if m.i >= len(m.responses) {
		return `{"action":"stop","confidence":0.9,"reason":"fixture stop","evidence_ids":[]}`, nil
	}
	r := m.responses[m.i]
	m.i++
	return r, nil
}

// TestFastClassBudgetRidesEveryCompletionOfTheSession — a FAST_AGENT session stamps the cap onto its
// context BEFORE the loop, so every completion it makes (an investigate cycle, a later decide call —
// all spend from the same session) carries it; a deep/standard/human-led/unclassified session carries
// NONE, so its requests are byte-identical to today's. Two completions per run (one tool cycle, then a
// stop) prove "every completion", not "the first one".
func TestFastClassBudgetRidesEveryCompletionOfTheSession(t *testing.T) {
	t.Setenv("TG_MODEL_MAX_TOKENS_FAST", "1500")
	run := func(class string) []int {
		deps := testDeps()
		probe := &capProbe{responses: []string{`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`}}
		deps.Model = probe
		if _, err := NewActivities(deps).InvestigateActivity(context.Background(), ingest.IncidentEnvelope{
			ExternalRef: "TG-42-budget", Host: "web01", AlertRule: "NginxDown",
			Severity: ingest.SeverityWarning, Site: "dc1",
		}, class, ClusterMemberContext{}); err != nil {
			t.Fatalf("class %q: %v", class, err)
		}
		return probe.caps
	}
	for _, tc := range []struct {
		class string
		want  int
	}{
		{string(execclass.FastAgent), 1500},
		{string(execclass.DeepInvestigation), 0},
		{string(execclass.StandardAgent), 0},
		{string(execclass.HumanLed), 0},
		{"", 0}, // unclassified: the legacy fallback is a deep/standard session — never a tightened budget
	} {
		caps := run(tc.class)
		if len(caps) < 2 {
			t.Fatalf("class %q: fixture must drive at least two completions, got %d", tc.class, len(caps))
		}
		for i, got := range caps {
			if got != tc.want {
				t.Errorf("class %q: completion %d carried cap %d, want %d", tc.class, i, got, tc.want)
			}
		}
	}
	// Unset env: even the fast class carries no cap — the mechanism ships INERT until an operator arms it.
	t.Setenv("TG_MODEL_MAX_TOKENS_FAST", "")
	for i, got := range run(string(execclass.FastAgent)) {
		if got != 0 {
			t.Errorf("unarmed: completion %d carried cap %d, want none — the knob must ship inert", i, got)
		}
	}
}
