package eval

import (
	"context"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/config"
)

// TestExecPhaseEval_OnBox runs the REPORT-ONLY execute-phase eval (TG-513) against the on-box model gateway
// and logs the scorecard. Like eval_integration_test.go it SKIPS unless TG_EVAL_GATEWAY (+ LITELLM_MASTER_KEY)
// is set, so `make all` / CI (no gateway) never runs it. It is a MEASUREMENT, not a gate: it asserts nothing
// about the scores; its only failure is a harness that could not run (no scenarios, or a gateway that never
// answered a single scenario). Both the agent turn and the judge turn use the `primary` tier — the same tier
// the investigate eval judges on.
func TestExecPhaseEval_OnBox(t *testing.T) {
	gwURL := os.Getenv("TG_EVAL_GATEWAY")
	if gwURL == "" {
		t.Skip("set TG_EVAL_GATEWAY (http://localhost:PORT via an SSH tunnel to dc1tg01:4000) + LITELLM_MASTER_KEY to run the execute-phase eval")
	}
	scenarios, err := LoadExecScenarios("execphase_scenarios.json")
	if err != nil {
		t.Fatalf("load scenarios: %v", err)
	}
	gw := model.NewGateway(gwURL, config.SecretRef("env:LITELLM_MASTER_KEY"))
	ctx := context.Background()
	complete := func(prompt string) (string, error) {
		return gw.Complete(ctx, "tg513-execphase", "primary", []model.Message{{Role: "user", Content: prompt}})
	}
	card, err := RunExecEval(scenarios, complete)
	if err != nil {
		t.Fatalf("RunExecEval: %v", err)
	}
	t.Logf("\n%s", card.Report())
	if card.N == 0 {
		t.Fatal("execute-phase eval measured 0 scenarios")
	}
	// A report is only trustworthy if MOST scenarios judged. A run where the majority silently failed
	// (gateway flake / judge-format drift) must surface as a harness failure, not read as "measured" over a
	// handful of rows — the a-check-that-cannot-report-nothing-to-check trap. Require >= 80% judged.
	if card.Judged*5 < card.N*4 {
		t.Fatalf("only %d of %d scenarios produced a parseable judge verdict (<80%%) — the gateway or judge-reply format is degraded; the report is not trustworthy", card.Judged, card.N)
	}
}
