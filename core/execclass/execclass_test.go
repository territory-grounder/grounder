package execclass

import "testing"

func TestClassifyRoutes(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want Class
	}{
		{"ambiguous event → human leads (dominates)", Input{Ambiguous: true, KnownPattern: true, Reversible: true}, HumanLed},
		{"correlated → deep", Input{Correlated: true, KnownPattern: true, Reversible: true}, DeepInvestigation},
		{"novel → deep", Input{Novel: true}, DeepInvestigation},
		{"known bounded procedure → deterministic (no agent)", Input{KnownProcedure: true, CriticalityTier: "service"}, Deterministic},
		{"read-only status → deterministic", Input{ReadOnly: true}, Deterministic},
		{"recurrent reversible isolated → fast", Input{KnownPattern: true, Reversible: true, CriticalityTier: "service"}, FastAgent},
		{"nothing special → standard", Input{}, StandardAgent},
		{"irreversible known-pattern → standard (no fast path)", Input{KnownPattern: true, Reversible: false}, StandardAgent},
	}
	for _, c := range cases {
		if got := Classify(c.in); got != c.want {
			t.Errorf("%s: Classify = %s, want %s", c.name, got, c.want)
		}
	}
}

// Fail-safe: a high-criticality (P0/host) target never earns a deterministic or fast shortcut, even when a
// known procedure / known pattern would otherwise route there.
func TestCriticalTargetNeverShortcut(t *testing.T) {
	for _, tier := range []string{"host", "P0", "critical"} {
		if got := Classify(Input{KnownProcedure: true, CriticalityTier: tier}); got == Deterministic {
			t.Errorf("tier %q known-procedure must NOT be deterministic (got %s)", tier, got)
		}
		if got := Classify(Input{KnownPattern: true, Reversible: true, CriticalityTier: tier}); got == FastAgent {
			t.Errorf("tier %q recurrent must NOT be fast-pathed (got %s)", tier, got)
		}
	}
}

// Fail-safe direction: correlated/novel outranks a known-pattern shortcut.
func TestCorrelatedOutranksKnownPattern(t *testing.T) {
	if got := Classify(Input{Correlated: true, KnownPattern: true, Reversible: true, CriticalityTier: "service"}); got != DeepInvestigation {
		t.Fatalf("a correlated incident must go deep even if it looks recurrent, got %s", got)
	}
}

func TestPathHelpers(t *testing.T) {
	if !SkipsAgent(Deterministic) || SkipsAgent(StandardAgent) {
		t.Fatal("only DETERMINISTIC skips the agent")
	}
	if !NeedsDeepContext(DeepInvestigation) || NeedsDeepContext(FastAgent) || NeedsDeepContext(Deterministic) {
		t.Fatal("only deep/standard warrant the full context build")
	}
	if !HumanOwnsDecision(HumanLed) || HumanOwnsDecision(StandardAgent) {
		t.Fatal("only HUMAN_LED hands the decision to the operator")
	}
	for _, c := range []Class{Deterministic, FastAgent, StandardAgent, DeepInvestigation, HumanLed} {
		if !Valid(c) {
			t.Errorf("%s must be valid", c)
		}
	}
	if Valid("nonsense") {
		t.Error("unknown class must be invalid")
	}
}
