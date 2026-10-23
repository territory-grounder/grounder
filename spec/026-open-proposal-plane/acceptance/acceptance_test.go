package acceptance

import (
	"testing"

	"github.com/cucumber/godog"
)

// TestOpenProposalPlaneAcceptance runs the spec/026 acceptance feature (the 020/017 harness pattern:
// "~@pending" + Strict). Stage-1 tasks (T-026-1..8) landed the plane's real code, so the scenarios those
// tasks own drop @pending and bind against the REAL paths — the one proposal grammar (core/proposal), the
// runner workflow's shadow divert driven through the Temporal test environment, and the structural
// never-executable chain (GateActivity seal → real interceptor → real empty-argv effect leaf). Scenarios
// whose oracles live in package tests or the console e2e suite (REQ-2603/2604/2605/2606/2607/2610/2611 —
// temporal/runner/shadow_propose_test.go, core/db/triage_judgment_test.go, core/httpapi/proposals_test.go,
// deploy/console/v2/e2e/proposals-honesty.mjs, core/proposal/evidence_test.go) stay @pending HERE: the
// mapping names their real oracles, and a godog re-drive would be a duplicate, not more coverage.
func TestOpenProposalPlaneAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "spec/026 open-proposal-plane",
		ScenarioInitializer: initializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"."},
			Tags:     "~@pending",
			Strict:   true,
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("spec/026 acceptance scenarios failed")
	}
}

// stepRegistrars — each task binds its own scenarios' steps via an init() append (the spec/017/020
// registrar pattern), so parallel task work never edits this shared harness.
var stepRegistrars []func(*godog.ScenarioContext)

func initializeScenario(sc *godog.ScenarioContext) {
	for _, register := range stepRegistrars {
		register(sc)
	}
}
