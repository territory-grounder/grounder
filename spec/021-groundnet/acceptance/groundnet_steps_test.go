package acceptance

import "github.com/cucumber/godog"

// T-021-* step stubs. This is a far-future Draft CONTRACT: no product code is built yet, so every scenario
// in groundnet.feature is @pending and skipped by the "~@pending" filter — these stubs never execute. They
// exist so each task T-021-* has a place to bind its scenarios' step definitions to the real core/groundnet
// code as it lands (the same registrar-append pattern spec/017 and spec/020 use, keeping the shared
// acceptance_test.go harness untouched by parallel task work). Until a task lands, its steps return
// godog.ErrPending.
func init() {
	stepRegistrars = append(stepRegistrars, registerGroundnetStubSteps)
}

// registerGroundnetStubSteps registers pending stubs for the envelope keystone scenario (T-021-1, REQ-2100).
// Each landed task replaces its stub with a registrar in its own <task>_steps_test.go that drives the real
// core/groundnet envelope + adapter seam.
func registerGroundnetStubSteps(sc *godog.ScenarioContext) {
	sc.Step(`^a wisdom chunk carrying id payload_version two_layer_marker producer_attestation signature provenance_chain verified_outcome_evidence and payload$`, pendingGroundnetStep)
	sc.Step(`^a node parses and validates the envelope$`, pendingGroundnetStep)
	sc.Step(`^the envelope fields and their invariants are the stable contract and the node validates the envelope independently of the payload it carries$`, pendingGroundnetStep)
}

// pendingGroundnetStep marks a step pending until its owning task T-021-* binds it to the real code.
func pendingGroundnetStep() error { return godog.ErrPending }
