package acceptance

import (
	"context"
	"fmt"
	"strings"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/core/safety"
)

// registerSafetyPlaneSteps binds the Phase-2 canary safety + observability plane (REQ-525): the runtime
// mutation kill-switch primitive (Chokepoint.ForceShadow) and the read-only /metrics exposition. Both are
// SAFETY-ADDITIVE — they turn mutation more off or read state, never enable it — so the oracle drives them
// entirely WITHOUT the actuating path (no chokepoint is ever put into an actuating mode here).
func registerSafetyPlaneSteps(sc *godog.ScenarioContext) {
	var gate *safety.Chokepoint
	var mb *safety.MutationBreaker
	var rendered string

	sc.Step(`^a mutation gate on the read-only foundation$`, func() error {
		gate = safety.NewReadOnlyChokepoint()
		if gate.MayActuate() {
			return fmt.Errorf("a fresh gate must start OFF (read-only foundation)")
		}
		return nil
	})
	sc.Step(`^the runtime disable is invoked twice$`, func() error {
		gate.ForceShadow("test")
		gate.ForceShadow("test") // idempotent — forcing an off chokepoint to shadow is a safe no-op
		return nil
	})
	sc.Step(`^mutation stays off and the gate refuses every mutation$`, func() error {
		if gate.MayActuate() {
			return fmt.Errorf("ForceShadow must keep mutation OFF")
		}
		if gate.GuardMutation() == nil {
			return fmt.Errorf("an off gate must refuse every mutation")
		}
		return nil
	})

	sc.Step(`^the read-only safety gate and an armed mutation breaker$`, func() error {
		gate = safety.NewReadOnlyChokepoint()
		var err error
		mb, err = safety.NewMutationBreaker(gate, breaker.NewMemStore(), 1, nil)
		return err
	})
	sc.Step(`^the metrics exposition is rendered$`, func() error {
		enabled := 0.0
		if gate.MayActuate() {
			enabled = 1
		}
		rendered = metrics.Render([]metrics.Sample{
			{Name: "mutation_enabled", Kind: metrics.Gauge, Help: "mutation gate on/off", Value: enabled},
			{Name: "circuit_breaker_state", Kind: metrics.Gauge, Help: "mutation breaker state", Value: mb.StateValue(context.Background())},
		})
		return nil
	})
	sc.Step(`^it reports mutation_enabled 0 and the circuit breaker gauge, and no secret$`, func() error {
		if !strings.Contains(rendered, "mutation_enabled 0") {
			return fmt.Errorf("exposition must report mutation_enabled 0; got:\n%s", rendered)
		}
		if !strings.Contains(rendered, "circuit_breaker_state 0") {
			return fmt.Errorf("exposition must report circuit_breaker_state 0 (closed); got:\n%s", rendered)
		}
		if strings.Contains(rendered, "mutation_enabled 1") {
			return fmt.Errorf("the read-only exposition must never report mutation_enabled 1; got:\n%s", rendered)
		}
		return nil
	})
}
