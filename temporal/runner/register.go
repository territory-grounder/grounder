package runner

// ActivityRegistry is the narrow registration seam RegisterActivities writes through. Both the
// production worker (go.temporal.io/sdk/worker.Worker) and the test environments
// (testsuite.TestWorkflowEnvironment / TestActivityEnvironment) satisfy it.
type ActivityRegistry interface {
	RegisterActivity(a interface{})
}

// RegisterActivities registers EVERY activity RunnerWorkflow can schedule — the ONE canonical list.
// The production worker and the eval/acceptance harnesses all register through this function, so a
// workflow-referenced activity missing from the composition root is structurally impossible.
//
// Provenance: on 2026-07-18 the FIRST prod session to reach a gated proposal stalled on
// ActivityNotRegisteredError (RecordPendingActivity), and the post-vote path then stalled again on
// ResolvePendingActivity — both were registered by the test harnesses but not by cmd/worker, so every
// test was green while prod was dark. register_test.go now proves by reflection that this list covers
// every *Activities method; a new activity that is not added here fails CI, not production.
func RegisterActivities(w ActivityRegistry, a *Activities) {
	w.RegisterActivity(a.SuppressActivity)
	// The incident CORRELATION stage (TG-169): the pre-context topology decision's evidence read and its
	// durable audit row. Unregistered, the workflow's step-0.75 dispatch stalls on a live terminus — the
	// exact 2026-07-18 failure class this list exists for.
	w.RegisterActivity(a.CorrelateActivity)
	w.RegisterActivity(a.InvestigateActivity)
	// TG-496 fix (c) — the deterministic guest-down auto-heal fast-path's proposal emitter. RunnerWorkflow
	// dispatches it INSTEAD of InvestigateActivity when the correlation stage confirmed an isolated,
	// non-critical, observed-stopped pve-liveness guest-down (cor.FastHeal). Read-only + pure synthesis (it
	// re-reads guest liveness and builds a start-guest proposal; it reaches NO effect leaf), so it stays off
	// the actuation queue — the EXECUTE step its proposal later reaches is ExecuteActivity, already routed there.
	w.RegisterActivity(a.DeterministicGuestHealActivity)
	w.RegisterActivity(a.AttributeActivity)
	w.RegisterActivity(a.ClassifyActivity)
	// TG-80 P2-6: the kill-terminal flip read + screen:killed ledger append. Triage plane only — it
	// touches no effect leaf; the session it ends never reaches the actuation queue at all.
	w.RegisterActivity(a.ScreenKillActivity)
	w.RegisterActivity(a.GateActivity)
	w.RegisterActivity(a.NotifyActivity)
	w.RegisterActivity(a.RecordVoteActivity)
	w.RegisterActivity(a.ExecuteActivity)
	w.RegisterActivity(a.VerifyActivity)
	w.RegisterActivity(a.ObserveClearedActivity)
	w.RegisterActivity(a.RecoveredSinceActivity)
	w.RegisterActivity(a.RecordPendingActivity)
	w.RegisterActivity(a.ResolvePendingActivity)
	w.RegisterActivity(a.RecordTriageActivity)
	// The shadow-proposal terminal (spec/026 REQ-2603): the open proposal plane's record+ledger+seam
	// activity, dispatched by the shadow divert branch. [Prior owner spec/012 T-012-1 (completed); this
	// line delivered under spec/026 T-026-3.]
	w.RegisterActivity(a.ShadowProposalActivity)
	w.RegisterActivity(a.MarkTriageClearedActivity)
	w.RegisterActivity(a.BackfillManifestActivity)
	w.RegisterActivity(a.ReconcileActivity)
	// The ladder credit is its OWN activity so the workflow can pin it to MaximumAttempts:1 — see
	// GraduationActivity. Registering it is not optional: the workflow dispatches it by reference, and an
	// unregistered activity fails at RUN time on a live terminus, not at build time.
	w.RegisterActivity(a.GraduationActivity)
	// The operator-facing MANUAL ROLLBACK lane (TG-462): the SEAL step (validate reversibility + seal the
	// inverse manifest + resolve approvers) runs on the triage queue; the EXECUTE step is registered here for
	// the `both` plane AND on the actuation queue below (it is the one rollback activity that reaches an effect
	// leaf, so it must be pinned to tg.actuate — RegisterActuationActivities). RollbackWorkflow dispatches both
	// by reference, so an unregistered one stalls at run time on a live terminus, never at build time.
	w.RegisterActivity(a.SealRollbackActivity)
	w.RegisterActivity(a.SealRollbackExecuteActivity)
	// spec/030 transaction-plan lane: the propose-terminal recipe compose (T-030-4), the durable rows
	// and the ledger narration (triage plane; the steps execute via the already-registered
	// ExecuteActivity, routed like any action).
	w.RegisterActivity(a.ComposePlanActivity)
	w.RegisterActivity(a.RecordPlanActivity)
	w.RegisterActivity(a.PlanTransitionActivity)
	w.RegisterActivity(a.PlanStepTransitionActivity)
	w.RegisterActivity(a.PlanEventActivity)
	// spec/029 T-029-2 — the armed revert's durable bookkeeping. Both are Postgres-only writes
	// (the row + the governance-ledger append): they gate/record the actuation but never touch an
	// estate credential, so like VerifyActivity they stay OFF the actuation queue. The arm is
	// dispatched by RunnerWorkflow before ExecuteActivity; the resolve by CommitConfirmWorkflow.
	w.RegisterActivity(a.ArmCommitConfirmActivity)
	w.RegisterActivity(a.ResolveCommitConfirmActivity)
	// spec/029 T-029-3 — the confirm/inverse arm. Consult and seal are Postgres/manifest reads +
	// a manifest seal (triage-plane, like the manual lane's SealRollbackActivity); the breaker
	// trip touches process/durable breaker state only. The one activity that reaches an effect
	// leaf — SealRollbackExecuteActivity — is ALREADY on both lists (TG-462), and the child
	// dispatches it onto tg.actuate exactly as the manual rollback does.
	w.RegisterActivity(a.ConsultCommitConfirmActivity)
	w.RegisterActivity(a.SealCommitConfirmInverseActivity)
	w.RegisterActivity(a.TripMutationBreakerActivity)
	// TG-483: the terminus collateral re-check — a durable-log READ (no effect leaf).
	w.RegisterActivity(a.ObserveCollateralActivity)
}

// RegisterActuationActivities registers the activities that MUTATE the estate, and ONLY those (TG-153). They
// are dispatched by RunnerWorkflow onto tg.TaskQueueActuate (see execCtx in workflow.go), so this is the set
// the ACTUATION-plane worker must register — and, symmetrically, the set that reaches a process holding the
// actuation SSH identity.
//
// ExecuteActivity is the whole list, and that is a claim worth stating rather than assuming: it is the only
// Runner activity that traverses the interceptor chain to an effect leaf (or the regime engine's LaneEffect),
// i.e. the only one that can reach sshactuation/awxjob/proxmox with a write credential. VerifyActivity,
// despite sitting beside it in the workflow, reads back a durable verdict row from Postgres and touches no
// estate credential; ObserveClearedActivity re-reads active alerts through the READ plane's alert reader.
// Both therefore stay on the triage queue where their credentials already live. A future activity that
// actuates must be added HERE as well as to RegisterActivities, or it will be scheduled onto a queue the
// actuation worker does not poll and stall — TestActuationActivitiesAreRegisteredAndRouted is the guard.
//
// In the DEFAULT `both` plane the SAME process registers this set on tg.actuate and the full set on
// tg.runner, so the dispatch lands in the same worker it always did and behaviour is unchanged.
func RegisterActuationActivities(w ActivityRegistry, a *Activities) {
	w.RegisterActivity(a.ExecuteActivity)
	// TG-462: the manual-rollback execute traverses the SAME interceptor chain to an effect leaf (or the regime
	// engine's LaneEffect) — it is the only OTHER Runner-package activity that can reach a write credential, so
	// like ExecuteActivity it must register on the actuation queue or a rollback would be scheduled onto a queue
	// the actuation worker does not poll and stall (TestActuationActivitiesAreRegisteredAndRouted guards this).
	w.RegisterActivity(a.SealRollbackExecuteActivity)
}
