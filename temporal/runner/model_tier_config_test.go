package runner

// THE MODEL-TIER ALIASES ARE CONFIG; THE SAFETY FLOOR IS NOT (TG-116).
//
// Which alias a component asks for is deployment policy — the alias→real-model mapping already lives in
// the gateway config, so retuning cost/latency should not require a Go change. But the BRANCH that sends a
// deep-investigation topology or a critical severity to the reasoner is a safety floor (MECH-402), and a
// floor an operator can flatten from .env is not a floor.
//
// So these oracles pin BOTH halves: the names are overridable, and the RULE is not.

import (
	"testing"

	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/ingest"
)

func TestTierNamesAreOverridableFromConfig(t *testing.T) {
	t.Setenv("TG_MODEL_TIER_INVESTIGATE", "arm-haiku")
	t.Setenv("TG_MODEL_TIER_DEEP", "arm-opus")
	t.Setenv("TG_MODEL_TIER_DECIDE", "judge")

	ordinary := ingest.IncidentEnvelope{Severity: ingest.SeverityWarning}
	if got := investigateTierFor(ordinary, string(execclass.StandardAgent)); got != "arm-haiku" {
		t.Errorf("ordinary incident tier = %q, want the configured alias — retuning cost/latency must not "+
			"require a Go change", got)
	}
	deep := ingest.IncidentEnvelope{Severity: ingest.SeverityWarning}
	if got := investigateTierFor(deep, string(execclass.DeepInvestigation)); got != "arm-opus" {
		t.Errorf("deep-investigation tier = %q, want the configured alias", got)
	}
	if got := decisionTierFor(); got != "judge" {
		t.Errorf("decision tier = %q, want the configured alias", got)
	}
}

// THE FLOOR ITSELF STAYS IN CODE. Configuring the names must not let a deep or critical incident be routed
// to the ordinary tier — that is the safety property, and it must hold under ANY naming.
func TestConfigCannotFlattenTheReasonerFloor(t *testing.T) {
	t.Setenv("TG_MODEL_TIER_INVESTIGATE", "cheap")
	t.Setenv("TG_MODEL_TIER_DEEP", "strong")

	deep := investigateTierFor(ingest.IncidentEnvelope{Severity: ingest.SeverityWarning}, string(execclass.DeepInvestigation))
	crit := investigateTierFor(ingest.IncidentEnvelope{Severity: ingest.SeverityCritical}, string(execclass.StandardAgent))
	if deep == "cheap" || crit == "cheap" {
		t.Fatalf("a deep (%q) or critical (%q) incident reached the ORDINARY tier — the reasoner floor is "+
			"a safety property (MECH-402), not a default an operator can override away", deep, crit)
	}
}

// An empty or whitespace-only value must fall back, never propagate "". An unset alias resolves at the
// gateway to no model at all, so a mistyped env var would become a silent total outage of the lane.
func TestBlankTierConfigFallsBackRatherThanPropagatingEmpty(t *testing.T) {
	t.Setenv("TG_MODEL_TIER_INVESTIGATE", "   ")
	t.Setenv("TG_MODEL_TIER_DECIDE", "")
	if got := investigateTierFor(ingest.IncidentEnvelope{Severity: ingest.SeverityWarning}, string(execclass.StandardAgent)); got != "fast" {
		t.Errorf("blank config produced %q, want the compiled default — an empty alias is not a model", got)
	}
	if got := decisionTierFor(); got != "primary" {
		t.Errorf("blank decide config produced %q, want the compiled default", got)
	}
}
