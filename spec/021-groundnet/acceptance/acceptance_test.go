package acceptance

import (
	"testing"

	"github.com/cucumber/godog"
)

// TestGroundnetContractAcceptance runs the spec/021 acceptance feature. This is a far-future Draft CONTRACT:
// no product code is built yet, so EVERY scenario in the feature is tagged @pending and is excluded by the
// "~@pending" filter — the runner executes zero scenarios and the suite passes vacuously. As tasks T-021-*
// land, their scenarios drop @pending, bind step definitions to the real core/groundnet envelope + adapter
// seam, and must then pass strictly. Until then this asserts the feature parses and the honest coverage
// frontier in _test_mapping.json holds (all pending — declared debt, INV-22).
func TestGroundnetContractAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "spec/021 groundnet",
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
		t.Fatal("spec/021 acceptance scenarios failed")
	}
}

// stepRegistrars lets each task T-021-* bind its OWN scenarios' step definitions in its own
// <task>_steps_test.go via an init() append — so parallel task work never edits this shared harness (the same
// pattern spec/017 and spec/020 use). A task whose code has not landed registers nothing (or a pending stub)
// and its scenarios stay @pending (skipped by the "~@pending" filter). As each task lands, it drops @pending
// from its scenarios and appends a registrar here that binds them against the real code.
var stepRegistrars []func(*godog.ScenarioContext)

// initializeScenario invokes every landed task's step registrar. Until a task lands, its scenarios are
// @pending and never execute.
func initializeScenario(sc *godog.ScenarioContext) {
	for _, register := range stepRegistrars {
		register(sc)
	}
}
