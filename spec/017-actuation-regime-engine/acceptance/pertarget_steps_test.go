package acceptance

import (
	"context"
	"fmt"

	"github.com/cucumber/godog"
	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/core/safety"
)

func init() { stepRegistrars = append(stepRegistrars, registerPerTargetSteps) }

// REQ-1717 (P3-B2): the native-ssh lane can resolve its effect leaf PER ACTION TARGET. These steps drive the
// REAL regime.LaneEffect + the spec/013 interceptor with a per-target lane (NewNativeSSHLaneFunc), proving the
// builder is called with the action's own target, the leaf executes exactly once under an actuating mode, and
// nothing executes under Shadow (the per-target lane is dormant until the owner-present flip, REQ-1707).
func registerPerTargetSteps(sc *godog.ScenarioContext) {
	var gotTarget string
	var actLeaf *recordingActuator
	var actOut, shadowOut actuate.Outcome
	var actErr, shadowErr error
	var shadowExecs int

	sc.Step(`^a per-target native-ssh lane that binds each action's leaf to its own target host$`, func() error {
		actLeaf = &recordingActuator{}
		gotTarget = ""
		return nil
	})
	sc.Step(`^the seam applies a governed actuation under an actuating mode and under Shadow$`, func() error {
		laneOn := regime.NewNativeSSHLaneFunc(func(_ context.Context, target string) (actuation.Actuator, error) {
			gotTarget = target
			return actLeaf, nil
		})
		seamOn := regime.NewLaneEffect(func(l actuation.Actuator) *actuate.Interceptor {
			return actuate.NewInterceptor(safety.NewActuatingChokepoint(), l, audit.NewLedger())
		})
		actOut, actErr = seamOn.Apply(context.Background(), laneOn, accGoodRequest())

		shadowLeaf := &recordingActuator{}
		laneOff := regime.NewNativeSSHLaneFunc(func(_ context.Context, _ string) (actuation.Actuator, error) {
			return shadowLeaf, nil
		})
		seamOff := regime.NewLaneEffect(func(l actuation.Actuator) *actuate.Interceptor {
			return actuate.NewInterceptor(safety.NewReadOnlyChokepoint(), l, audit.NewLedger())
		})
		shadowOut, shadowErr = seamOff.Apply(context.Background(), laneOff, accGoodRequest())
		shadowExecs = shadowLeaf.execs
		return nil
	})
	sc.Step(`^the leaf executes once on the action's target under actuating mode and is refused before execute under Shadow$`, func() error {
		if actErr != nil {
			return fmt.Errorf("actuating apply errored: %w", actErr)
		}
		if gotTarget != "web01" {
			return fmt.Errorf("the per-target builder must be called with the action's target (web01), got %q", gotTarget)
		}
		if !actOut.Executed || actOut.Refused || actLeaf.execs != 1 {
			return fmt.Errorf("under an actuating mode the per-target leaf must execute exactly once on the target, got %+v execs=%d", actOut, actLeaf.execs)
		}
		if shadowErr != nil {
			return fmt.Errorf("shadow apply errored: %w", shadowErr)
		}
		if shadowOut.Executed || shadowExecs != 0 {
			return fmt.Errorf("under Shadow the per-target leaf must be refused before execute (dormant), got %+v execs=%d", shadowOut, shadowExecs)
		}
		return nil
	})
}
