package main

import (
	"os"
	"strings"
	"testing"
)

// Composition-root guard (TG-501): pin that wireSkillCrons still wires all FOUR crons — skill-trial
// finalizer, session judge, skill generator, and escalation FireDue — so the god-file carve that extracted
// it from main() cannot silently drop one. Each is a fire-and-forget Temporal workflow/activity
// registration plus an idempotent cron start, none of it observable from outside the package, so — the
// same reasoning worker_wiring_inventory_test.go and worker_model_budget_test.go rely on — the guard reads
// the source as text and asserts the wiring, rather than exercising a live Temporal server.
func TestWireSkillCronsWiresAllFourCrons(t *testing.T) {
	src, err := os.ReadFile("skill_cron_wiring.go")
	if err != nil {
		t.Fatalf("read skill_cron_wiring.go: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		`w.RegisterWorkflow(skilltrial.FinalizerWorkflow)`,
		`ID: skilltrial.WorkflowID, TaskQueue: tg.TaskQueueRunner, CronSchedule: skilltrial.CronSchedule,`,
		`w.RegisterWorkflow(skilljudge.JudgeWorkflow)`,
		`ID: skilljudge.WorkflowID, TaskQueue: tg.TaskQueueRunner, CronSchedule: skilljudge.CronSchedule,`,
		`w.RegisterWorkflow(skillgen.GeneratorWorkflow)`,
		`ID: skillgen.WorkflowID, TaskQueue: tg.TaskQueueRunner, CronSchedule: skillgen.CronSchedule,`,
		`escActs := &escsched.Activities{D: escsched.Deps{Controller: escalationController}}`,
		`w.RegisterWorkflow(escsched.FireDueWorkflow)`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wireSkillCrons no longer wires %q — a skill/escalation cron was dropped in the carve", want)
		}
	}
}

// main() must actually CALL the extracted constructor — a carve that leaves it uncalled is a dark seam
// (present in the tree, absent from the process; the same class TG-315's authlog collector shipped as).
func TestMainCallsWireSkillCrons(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(mainSrc), "wireSkillCrons(w, c, skillTrialActs, skillJudgeActs, skillGenActs, escalationController)") {
		t.Error("main.go no longer calls wireSkillCrons(w, c, skillTrialActs, skillJudgeActs, skillGenActs, escalationController) — the extracted skill-cron wiring is unreferenced")
	}
}
