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
	w.RegisterActivity(a.InvestigateActivity)
	w.RegisterActivity(a.AttributeActivity)
	w.RegisterActivity(a.ClassifyActivity)
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
	w.RegisterActivity(a.BackfillManifestActivity)
	w.RegisterActivity(a.ReconcileActivity)
}
