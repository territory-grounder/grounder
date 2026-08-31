package acceptance

import (
	"testing"

	"github.com/cucumber/godog"
)

// TestEarnedOpClassCatalogAcceptance runs the spec/028 acceptance feature (the 017/020/026 harness pattern:
// "~@pending" + Strict).
//
// Only the scenarios whose oracles live HERE drop @pending. That is the honest frontier and it is
// deliberately narrow: most of this spec's scenarios are already driven by real oracles in the packages that
// own the code — core/actuate/opschema (admission, the laundering tripwire, the tamper drop),
// core/opclasscat (clustering, the stale-intake refusal), core/policy (the ladder truth table, the overlay
// ceiling), core/httpapi (the ratify lane), and deploy/console/v2/e2e/candidates-ratify.mjs (the empty form,
// the five questions, the one ladder). Re-driving those through godog would add a second caller, not a
// second proof, and a mapping row that pointed here instead of at them would be less true, not more.
//
// What this harness adds is the two CROSS-PACKAGE claims no single package can make on its own:
//
//	REQ-2805  an op-class absent from EVERY registry surface — embedded and overlay — seals to nothing
//	          executable, proven end-to-end through the real gate, the real interceptor and the real effect
//	          leaf rather than by asserting a lookup miss and calling it a chain.
//	REQ-2810  a FORECAST-lane verdict never feeds graduation. The forecast lane (core/falsify) and the
//	          ladder (core/policy) do not import each other, which is exactly why the claim needs an oracle
//	          that holds both at once: "these two packages are unconnected" is invisible from inside either.
func TestEarnedOpClassCatalogAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "spec/028 earned-op-class-catalog",
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
		t.Fatal("spec/028 acceptance scenarios failed")
	}
}

// stepRegistrars — each task binds its own scenarios' steps via an init() append (the spec/017/020/026
// registrar pattern), so parallel task work never edits this shared harness.
var stepRegistrars []func(*godog.ScenarioContext)

func initializeScenario(sc *godog.ScenarioContext) {
	for _, register := range stepRegistrars {
		register(sc)
	}
}
