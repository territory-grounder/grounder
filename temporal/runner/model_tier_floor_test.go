package runner

import (
	"testing"

	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/ingest"
)

// TestCriticalSeverityNeverInvestigatesOnTheCheapTier is the model-tier safety floor (MECH-402).
//
// The predecessor keys this on the risk BAND, which it has before launching. TG investigates first and
// classifies afterwards, so the floor keys on the strongest pre-loop signals available.
//
// TG-169 SPLIT ONE CONDITION INTO TWO, AND THIS TEST IS WHY IT HAD TO. The floor used to reach severity
// THROUGH the execution class, because `Correlated` was literally `severity == critical` — so "deep
// topology" and "critical incident" were one condition wearing two names. The correlation stage now
// (correctly) calls a lone critical ISOLATED, which routes it to STANDARD_AGENT. If the floor had been
// left keyed on the class alone, this accuracy fix would have DELETED the MECH-402 floor for every lone
// critical as a side effect, silently, with every test still green.
//
// KILLING MUTATION (executed): delete the `env.Severity == SeverityCritical` branch from
// investigateTierFor. RED here — a critical incident with no correlated peers investigates on the cheap
// tier, which is the safety floor gone, visible only on the day the two tiers stop pointing at the same
// model.
func TestCriticalSeverityNeverInvestigatesOnTheCheapTier(t *testing.T) {
	critical := ingest.IncidentEnvelope{ExternalRef: "x1", Host: "h", AlertRule: "r", Severity: ingest.SeverityCritical}
	if got := legacyExecClassFor(critical); got != execclass.DeepInvestigation {
		t.Fatalf("critical severity must classify DEEP_INVESTIGATION under the fallback rule, got %v — the "+
			"pre-correlation default a deployment with no durable pool still routes on is broken", got)
	}
	// The floor holds for a lone critical the correlator has (correctly) called isolated.
	if tier := investigateTierFor(critical, string(execclass.StandardAgent)); tier != "primary" {
		t.Fatalf("a CRITICAL incident must investigate on the primary tier even when it is NOT correlated, got %q", tier)
	}
	if tier := investigateTierFor(critical, ""); tier != "primary" {
		t.Fatalf("a CRITICAL incident must investigate on the primary tier, got %q", tier)
	}

	// And an ordinary incident must NOT be silently upgraded: the floor is a floor, not a blanket
	// upgrade that would quietly multiply the cost of every session.
	ordinary := ingest.IncidentEnvelope{ExternalRef: "x2", Host: "h", AlertRule: "r", Severity: ingest.SeverityWarning}
	if tier := investigateTierFor(ordinary, ""); tier != "fast" {
		t.Fatalf("a non-critical incident must stay on the fast tier, got %q", tier)
	}
}

// TestExecClassHasAConsumer pins the thing that made the classification worth computing at all. The exec
// class was computed at the top of the workflow and read by NOTHING (TG-210); the model-tier floor was its
// first real consumer.
//
// IT NOW HAS TO BE PROVED ON THE CLASS ALONE (TG-169). The old version of this test compared a CRITICAL
// envelope with a WARNING one, which was a valid proof only while the class was a pure function of
// severity — it would keep passing on the severity branch alone while the correlation stage's answer was
// ignored entirely. So the two envelopes here are IDENTICAL warnings and only the decided class differs:
// this is the false negative the ticket is about, a multi-host cascade built of warnings, which used to be
// handed to the cheapest reasoning TG has.
//
// KILLING MUTATION (executed): make investigateTierFor ignore its execClass argument (drop the classFor
// branch). RED — a correlated warning cascade and an isolated warning read on the same cheap tier, so the
// correlation stage changes nothing about how the incident is actually investigated and is decoration.
func TestExecClassHasAConsumer(t *testing.T) {
	warning := ingest.IncidentEnvelope{ExternalRef: "x3", Host: "h", AlertRule: "r", Severity: ingest.SeverityWarning}
	cascade := investigateTierFor(warning, string(execclass.DeepInvestigation))
	isolated := investigateTierFor(warning, string(execclass.StandardAgent))
	if cascade == isolated {
		t.Fatalf("the decided exec class must CHANGE something: a correlated WARNING cascade reads on %q and "+
			"an isolated warning on %q — identical, so the correlation stage is computed and discarded", cascade, isolated)
	}
	if cascade != "primary" {
		t.Fatalf("a correlated multi-host cascade must investigate on the reasoning tier, got %q — this is the "+
			"HuggingFace-shaped signal (many weak signals, no single critical) going to the cheapest model", cascade)
	}
}
