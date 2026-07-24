package acceptance

import "github.com/cucumber/godog"

// T-020-* step stubs. This is the SDD design gate: no product code is built yet, so every scenario in
// decision-tracer.feature is @pending and skipped by the "~@pending" filter — these stubs never execute.
// They exist so each task T-020-* has a place to bind its scenarios' step definitions to the real code as it
// lands (the same registrar-append pattern spec/017 uses, keeping the shared acceptance_test.go harness
// untouched by parallel task work). Until a task lands, its steps return godog.ErrPending.
func init() {
	stepRegistrars = append(stepRegistrars, registerTracerStubSteps)
}

// registerTracerStubSteps registers pending stubs for the Tier-0 keystone scenario (T-020-1). Each landed
// task replaces its stub with a registrar in its own <task>_steps_test.go that drives the real code.
func registerTracerStubSteps(sc *godog.ScenarioContext) {
	sc.Step(`^a proposal whose model confidence is parsed as a non-zero scalar$`, pendingTracerStep)
	sc.Step(`^the action is sealed and the decision record is read back from the real database$`, pendingTracerStep)
	sc.Step(`^the confidence scalar is threaded into the execute request and persisted and reads back non-zero rather than clamped to zero at the database boundary$`, pendingTracerStep)
}

// pendingTracerStep marks a step pending until its owning task T-020-* binds it to the real code.
func pendingTracerStep() error { return godog.ErrPending }
