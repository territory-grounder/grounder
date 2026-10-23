package regime

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
)

// actuation_limit_seam_test.go — TG-166(a) where it is most fragile.
//
// LaneEffect.Apply calls `ic := e.build(leaf)` on EVERY actuation: the routed path constructs a BRAND NEW
// spec/013 interceptor per action. Since NewInterceptor installs its own default actuation-frequency governor
// (so no wiring site can forget the control), the routed path would otherwise hand each actuation a FRESH,
// EMPTY window — and a rate limiter whose window is reset before every action limits precisely nothing. It
// would look armed at every level: the gate is in the chain, the ledger shows it passing, the drill matrix
// proves it can refuse — and the estate could still be restarted in a loop.
//
// That makes "the builder passes the SHARED limiter" the single most load-bearing wiring line in TG-166a, and
// this is where it is asserted: against the real seam, driving the real per-call construction, rather than
// against the composition root (which no test can reach).

// limiterBuilder is the shape cmd/worker's interceptorBuilder must have: a per-lane chain built fresh on every
// Apply, but handed the ONE governor the whole worker shares.
func limiterBuilder(shared *actuate.ActuationLimiter) InterceptorBuilder {
	return func(l actuation.Actuator) *actuate.Interceptor {
		return withPermissivePolicy(
			actuate.NewInterceptor(safety.NewActuatingChokepoint(), l, audit.NewLedger()),
		).WithActuationLimiter(shared)
	}
}

// KILLING MUTATION (executed 2026-08-04): drop `.WithActuationLimiter(shared)` from limiterBuilder — exactly
// the regression of deleting `ic = ic.WithActuationLimiter(bActuationLimiter)` from cmd/worker/main.go's
// interceptorBuilder, and a tempting one, because the constructor already installs a limiter so the line reads
// as redundant. All four actuations then run and this test FAILS at its vacuity floor with
//
//	"nothing was throttled on the routed path — the oracle matched nothing and would pass vacuously"
//
// with the execs assertion below naming the consequence: LaneEffect.Apply builds a FRESH interceptor per
// action, so an unshared governor starts every actuation with an empty window and the rate limit never fires
// on the routed path — the path production actually uses. Restored → green.
func TestRoutedLaneSharesOneActuationBudgetAcrossFreshInterceptors(t *testing.T) {
	leaf := &fakeActuator{cap: "ssh", ro: false}
	lane := NewNativeSSHLane(leaf)
	shared := actuate.NewActuationLimiter(nil).WithLimits(actuate.ActuationLimits{
		SessionPerWindow: 99, TargetPerWindow: 2, SessionInFlight: 1, TargetInFlight: 1,
	})
	seam := NewLaneEffect(limiterBuilder(shared))

	throttled := 0
	for n := 0; n < 4; n++ {
		out, err := seam.Apply(context.Background(), lane, goodRequest(t))
		if err != nil {
			t.Fatalf("attempt %d: Apply must not error on a wired seam: %v", n, err)
		}
		if out.RateLimited {
			throttled++
			if !strings.Contains(out.Reason, actuate.RefusalRateLimited) {
				t.Fatalf("attempt %d: a throttled routed actuation must say so, got %q", n, out.Reason)
			}
		}
	}

	// VACUITY FLOOR: this oracle counts throttled attempts. A run in which the chain refused everything for an
	// unrelated reason — an unwired collaborator, a gate above the limiter — would leave execs=0 and throttled=0
	// and prove nothing at all about the governor.
	if leaf.execs == 0 {
		t.Fatal("the routed path never reached the effect leaf — every attempt was refused before the limiter, " +
			"so this oracle measured nothing")
	}
	if throttled == 0 {
		t.Fatal("nothing was throttled on the routed path — the oracle matched nothing and would pass vacuously")
	}
	if leaf.execs != 2 {
		t.Fatalf("the routed lane actuated web01 %d time(s) against a per-target budget of 2. LaneEffect.Apply "+
			"builds a FRESH interceptor per action, so an unshared governor starts every actuation with an empty "+
			"window and the rate limit never fires on the routed path — the path production actually uses.", leaf.execs)
	}
}
