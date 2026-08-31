package main

import (
	"os"
	"strings"
	"testing"
)

// Guarding the wire function is not guarding the WIRING — the house pattern (cf.
// wireCoOccurrencePersist / wireAbandonedDecisionReap): the body may live in this file, but main.go must
// still call it, or all four collaborators are constructed by nothing and the actuation chain silently
// loses its per-execution record, its gate trail, its pre-mutation capture and its cross-process target
// admission at once.
//
// KILLING MUTATION: delete the wireActuationCollaborators(pool) call from main.go — RED here, where a
// build would stay green (the four variables would simply keep their nil zero values, which every seam
// treats as "unarmed" and none treats as an error).
func TestActuationCollaboratorsAreWiredFromMain(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := stripGoComments(string(raw))
	if len(src) < 10_000 {
		t.Fatal("main.go read back implausibly small — this guard would be vacuous")
	}
	if !strings.Contains(src, "wireActuationCollaborators(pool)") {
		t.Error("main.go does not call wireActuationCollaborators(pool) — the per-execution record, the " +
			"gate-verdict trail, the TG-58 pre-state capture and the TG-81 b2 target admission would all be nil, " +
			"and every one of those seams reads nil as 'unarmed' rather than as an error")
	}
	// The four must be handed to the DIRECT interceptor too, not merely constructed.
	for _, want := range []string{"WithExecutionSink(bExecutionSink)", "WithGateVerdictSink(bGateVerdict)",
		"WithPreStateSink(bPreStateSink)", "WithTargetAdmission(bTargetAdmission)"} {
		if !strings.Contains(src, want) {
			t.Errorf("main.go no longer passes %s — the collaborator is built and then dropped", want)
		}
	}
}

// The boot line is the OPERATOR-facing half of the fix: two of these controls shipped with no boot
// evidence at all, so "is it armed?" could not be answered from the log. A wiring change that keeps the
// call but drops the line would restore that blindness silently.
//
// KILLING MUTATION: delete the log.Print in wireActuationCollaborators — RED.
func TestTheCollaboratorBootLineNamesEveryControl(t *testing.T) {
	raw, err := os.ReadFile("actuation_collaborators_wiring.go")
	if err != nil {
		t.Fatalf("read wiring file: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "log.Print") {
		t.Fatal("the wiring emits no boot line — an armed control that says nothing at boot is indistinguishable " +
			"from a dark one in the only record an operator reads")
	}
	for _, want := range []string{"action_execution", "interceptor_gate_verdict", "action_prestate", "actuation_target_state"} {
		if !strings.Contains(src, want) {
			t.Errorf("the boot line does not name %s — a control the log omits is one nobody can confirm is armed", want)
		}
	}
}
