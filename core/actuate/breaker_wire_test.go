package actuate

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/safety"
)

// TestBreakerWireIsInertUnderMutationOff proves the armed-breaker wire is present but does NOTHING while the
// mode is Shadow: Do refuses at the mode chokepoint long before any execution, so the breaker is never tripped,
// its gauge stays closed (0), and the chokepoint stays read-only. This is the whole point — the
// breaker→chokepoint wire is built and armed but inert until a canary escalates the mode. (No mode transition.)
func TestBreakerWireIsInertUnderMutationOff(t *testing.T) {
	ctx := context.Background()
	cp := safety.NewReadOnlyChokepoint() // mode Shadow (read-only)
	mb, err := safety.NewMutationBreaker(cp, breaker.NewMemStore(), 1, nil)
	if err != nil {
		t.Fatalf("arm breaker: %v", err)
	}
	act := &fakeActuator{}
	i := wired(cp, act).WithMutationBreaker(mb)

	out, err := i.Do(ctx, goodRequest(t))
	if err != nil {
		t.Fatalf("Do returned a fail-loud error under mode Shadow: %v", err)
	}
	if !out.Refused || act.execs != 0 {
		t.Fatalf("mode Shadow must refuse and NOT execute: %+v execs=%d", out, act.execs)
	}
	if cp.MayActuate() {
		t.Fatal("the chokepoint must remain read-only")
	}
	if mb.StateValue(ctx) != 0 {
		t.Fatalf("the breaker must remain closed (0) — never tripped under mutation OFF, got %v", mb.StateValue(ctx))
	}
	if mb.Deviations() != 0 {
		t.Fatalf("no deviation may be recorded under mutation OFF, got %d", mb.Deviations())
	}
}

// TestCrossProcessBreakerTripForceShadowsSibling proves the design-wisdom #3 SYSTEM-WIDE kill through the REAL
// interceptor (REQ-1210): worker-1 trips its breaker over a SHARED store; worker-2 — a separate actuating
// interceptor armed with its OWN breaker over the SAME store, holding a fully-admissible request that WOULD
// execute if the shared breaker were closed — instead REFUSES and force-Shadows its own chokepoint the moment it
// consults the shared breaker before actuating. A breaker.MemStore shared by two breaker values is exactly two
// sibling worker processes coordinating through the one durable mutation_breaker_state row. Mutation stays OFF
// (nothing executes). This is the guarantee a per-process breaker never delivered.
func TestCrossProcessBreakerTripForceShadowsSibling(t *testing.T) {
	ctx := context.Background()
	shared := breaker.NewMemStore() // the one durable row both workers read/write (pgx row twin)

	// Worker-1 trips its breaker (a deviation) — opens the SHARED row.
	cp1 := safety.NewActuatingChokepoint()
	mb1, err := safety.NewMutationBreaker(cp1, shared, 1, nil)
	if err != nil {
		t.Fatalf("arm breaker 1: %v", err)
	}
	if disabled, err := mb1.Trip(ctx, "deviation on worker-1"); err != nil || !disabled {
		t.Fatalf("worker-1 trip = (disabled=%v, err=%v), want (true, nil)", disabled, err)
	}

	// Worker-2: a fresh actuating interceptor armed with its OWN breaker over the SAME shared store.
	cp2 := safety.NewActuatingChokepoint()
	act := &fakeActuator{}
	mb2, err := safety.NewMutationBreaker(cp2, shared, 1, nil)
	if err != nil {
		t.Fatalf("arm breaker 2: %v", err)
	}
	i := wired(cp2, act).WithMutationBreaker(mb2)
	if !cp2.MayActuate() {
		t.Fatal("precondition: worker-2 must be actuating before it honors the cross-process trip")
	}

	out, err := i.Do(ctx, goodRequest(t))
	if err != nil {
		t.Fatalf("Do returned a fail-loud error: %v", err)
	}
	if !out.Refused || act.execs != 0 {
		t.Fatalf("worker-2 must REFUSE a fully-admissible mutation after a sibling tripped the shared breaker: %+v execs=%d", out, act.execs)
	}
	if cp2.MayActuate() {
		t.Fatal("worker-2 must be force-Shadowed (read-only) after honoring the cross-process trip")
	}
}
