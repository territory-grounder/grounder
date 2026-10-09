package runner

import (
	"testing"

	"github.com/territory-grounder/grounder/core/ingest"
)

// deepEnv is an envelope that execClassFor routes to DeepInvestigation, so MECH-402's floor applies.
func deepEnv(t *testing.T) ingest.IncidentEnvelope {
	t.Helper()
	return ingest.IncidentEnvelope{
		ExternalRef: "arm-test", AlertRule: "Service-up/down", Severity: ingest.SeverityCritical,
		Host: "dc1pve01", Site: "dc1",
	}
}

// KILLING MUTATION: read TG_EVAL_ARM_INVESTIGATE without requiring TG_EVAL_ARM. RED — the model-tier
// SAFETY FLOOR (MECH-402) routes a deep investigation up to the reasoner, and a single environment
// variable able to route it back down is a way to weaken that floor in production silently. The override
// must require a worker to declare itself an experiment first.
func TestTheArmOverrideIsInertWithoutAnExplicitEvalArm(t *testing.T) {
	t.Setenv("TG_EVAL_ARM_INVESTIGATE", "arm-haiku")
	t.Setenv("TG_EVAL_ARM_DECIDE", "arm-haiku")
	// TG_EVAL_ARM deliberately NOT set — this is a production-shaped process.
	if got := investigateTierFor(deepEnv(t), ""); got == "arm-haiku" {
		t.Fatal("a single env var lowered the MECH-402 investigation floor with no eval-arm declaration")
	}
	if got := decisionTierFor(); got != "primary" {
		t.Fatalf("decision tier=%q without an eval arm, want primary", got)
	}
}

// With the arm declared, the override applies — otherwise the experiment cannot be run at all.
func TestAnExplicitEvalArmOverridesBothTiers(t *testing.T) {
	t.Setenv("TG_EVAL_ARM", "ARM-CHEAP")
	t.Setenv("TG_EVAL_ARM_INVESTIGATE", "arm-haiku")
	t.Setenv("TG_EVAL_ARM_DECIDE", "arm-haiku")
	if got := investigateTierFor(deepEnv(t), ""); got != "arm-haiku" {
		t.Fatalf("investigate tier=%q, want arm-haiku", got)
	}
	if got := decisionTierFor(); got != "arm-haiku" {
		t.Fatalf("decision tier=%q, want arm-haiku", got)
	}
}

// An arm that names no tiers must fall through to production behaviour, not to empty strings.
func TestAnEmptyArmFallsThroughToTheProductionTiers(t *testing.T) {
	t.Setenv("TG_EVAL_ARM", "ARM-CONTROL")
	if got := investigateTierFor(deepEnv(t), ""); got != "primary" {
		t.Fatalf("investigate tier=%q on a deep incident, want the MECH-402 floor 'primary'", got)
	}
	if got := decisionTierFor(); got != "primary" {
		t.Fatalf("decision tier=%q, want primary", got)
	}
}
