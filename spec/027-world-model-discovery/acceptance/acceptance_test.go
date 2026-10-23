package acceptance

import (
	"testing"

	"github.com/cucumber/godog"
)

// TestWorldModelDiscoveryAcceptance is spec/027's FIRST runner (the 017/020/026/028 harness pattern:
// "~@pending" + Strict). Added 2026-08-03 under TG-227.
//
// Until this existed the spec's mapping was UNEXECUTED — ten scenarios, every one @pending, and no suite
// that would even fail to run them. That mattered more here than anywhere: spec/027 is the earned
// op-class epic's ALLOWLIST SOURCE — it decides which hosts, units and containers a ratified class may
// touch — and TG-227 just made ratification reachable, so the hazards these scenarios name are now
// constructible in production. A spec whose oracles cannot run is a spec whose guarantees are vintage.
//
// Every scenario is still @pending, so this suite currently runs ZERO of them — deliberately. What it
// changes: the harness compiles in CI, the registrar seam below is ready, and the first de-pended
// scenario needs a step file and nothing else. The frontier priority is REQ-2707 ("A ratified class
// without adopted targets polls but cannot touch the host"): two independent grants — ratify AND adopt —
// must both exist before a leaf acts, and with ratification now live, that intersection is the next
// property whose absence would be invisible until it hurts someone.
func TestWorldModelDiscoveryAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "spec/027 world-model-discovery",
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
		t.Fatal("spec/027 acceptance scenarios failed")
	}
}

// stepRegistrars — each task binds its own scenarios' steps via an init() append (the shared-harness
// registrar pattern), so parallel task work never edits this file.
var stepRegistrars []func(*godog.ScenarioContext)

func initializeScenario(sc *godog.ScenarioContext) {
	for _, register := range stepRegistrars {
		register(sc)
	}
}
