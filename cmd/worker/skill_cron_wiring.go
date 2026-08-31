package main

import (
	"context"
	"errors"
	"log"

	enumspb "go.temporal.io/api/enums/v1"
	serviceerror "go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	coreesc "github.com/territory-grounder/grounder/core/escalation"
	tg "github.com/territory-grounder/grounder/temporal"
	escsched "github.com/territory-grounder/grounder/temporal/escalation"
	"github.com/territory-grounder/grounder/temporal/skillgen"
	"github.com/territory-grounder/grounder/temporal/skilljudge"
	"github.com/territory-grounder/grounder/temporal/skilltrial"
)

// wireSkillCrons registers the skill-flywheel finalizer/judge/generator workflows and their idempotent
// crons, plus the escalation FireDue cron, carved out of main()'s composition root (TG-501 LOC-debt
// paydown): each arm is a no-op unless its Activities/Controller was already constructed. Behaviour is
// unchanged by the move.
func wireSkillCrons(
	w planeWorker,
	c client.Client,
	skillTrialActs *skilltrial.Activities,
	skillJudgeActs *skilljudge.Activities,
	skillGenActs *skillgen.Activities,
	escalationController *coreesc.Controller,
) {
	if skillTrialActs != nil {
		w.RegisterWorkflow(skilltrial.FinalizerWorkflow)
		w.RegisterActivity(skillTrialActs.FinalizeActivity)
		// The finalizer CRON: idempotent start — an already-running cron is the desired state.
		if _, serr := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
			ID: skilltrial.WorkflowID, TaskQueue: tg.TaskQueueRunner, CronSchedule: skilltrial.CronSchedule,
			WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		}, skilltrial.FinalizerWorkflow); serr != nil {
			var started *serviceerror.WorkflowExecutionAlreadyStarted
			if !errors.As(serr, &started) {
				log.Printf("skill-trial finalizer: cron start failed: %v (trials will not finalize until next boot)", serr)
			}
		} else {
			log.Printf("skill-trial finalizer: cron armed (%s)", skilltrial.CronSchedule)
		}
	}
	if skillJudgeActs != nil {
		w.RegisterWorkflow(skilljudge.JudgeWorkflow)
		w.RegisterActivity(skillJudgeActs.JudgeBatchActivity)
		// The judge CRON: idempotent start — an already-running cron is the desired state.
		if _, serr := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
			ID: skilljudge.WorkflowID, TaskQueue: tg.TaskQueueRunner, CronSchedule: skilljudge.CronSchedule,
			WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		}, skilljudge.JudgeWorkflow); serr != nil {
			var started *serviceerror.WorkflowExecutionAlreadyStarted
			if !errors.As(serr, &started) {
				log.Printf("session judge: cron start failed: %v (sessions will not be judged until next boot)", serr)
			}
		} else {
			log.Printf("session judge: cron armed (%s) — judging up to %d sessions per run", skilljudge.CronSchedule, skilljudge.BatchLimit)
		}
	}
	if skillGenActs != nil {
		// Distinctly-named workflow (the bare-function-name collision guard lives in
		// temporal/skilltrial/finalizer_names_test.go — GeneratorWorkflow is on that list).
		w.RegisterWorkflow(skillgen.GeneratorWorkflow)
		w.RegisterActivity(skillGenActs.GenerateActivity)
		// The generator CRON: idempotent start — an already-running cron is the desired state.
		if _, serr := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
			ID: skillgen.WorkflowID, TaskQueue: tg.TaskQueueRunner, CronSchedule: skillgen.CronSchedule,
			WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		}, skillgen.GeneratorWorkflow); serr != nil {
			var started *serviceerror.WorkflowExecutionAlreadyStarted
			if !errors.As(serr, &started) {
				log.Printf("skill generator: cron start failed: %v (candidates will not be generated until next boot)", serr)
			}
		} else {
			log.Printf("skill generator: cron armed (%s) — generate-only, competence-plane; never actuates", skillgen.CronSchedule)
		}
	}
	if escalationController != nil {
		// The escalation FireDue CRON (spec/003 wired, Gulli ch12): fires every DUE re-check so an enqueued
		// escalation actually re-escalates/pages/stands down. Distinctly-named workflow (Temporal registers by
		// bare function name). Idempotent start — an already-running cron is the desired state. A FireDue error
		// is captured in the activity Result and never crashes the worker.
		escActs := &escsched.Activities{D: escsched.Deps{Controller: escalationController}}
		w.RegisterWorkflow(escsched.FireDueWorkflow)
		w.RegisterActivity(escActs.FireDueActivity)
		if _, serr := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
			ID: escsched.WorkflowID, TaskQueue: tg.TaskQueueRunner, CronSchedule: escsched.CronSchedule,
			WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		}, escsched.FireDueWorkflow); serr != nil {
			var started *serviceerror.WorkflowExecutionAlreadyStarted
			if !errors.As(serr, &started) {
				log.Printf("escalation FireDue: cron start failed: %v (enqueued escalations will not fire until next boot)", serr)
			}
		} else {
			log.Printf("escalation FireDue: cron armed (%s) — fires due re-checks/re-escalations/pages; never actuates", escsched.CronSchedule)
		}
	}
}
