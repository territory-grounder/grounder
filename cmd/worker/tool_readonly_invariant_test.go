package main

// TG-49 — THE READ-ONLY INVARIANT THE BATCHED DISPATCH RESTS ON, ASSERTED OVER THE LIVE SET.
//
// agent.Run's batched directive (TG-49) dispatches up to agent.MaxBatchTools calls CONCURRENTLY. That is
// safe to offer at all because every tool the agent can name is read-only BY CONSTRUCTION: the actuation
// plane registers no agent tools, and agent.ToolSet.RegisterFrom refuses any tool whose ReadOnly() is
// false — so the concurrent lane can never be handed a mutation to parallelize. This control asserts that
// invariant where it is actually decided: over the tool families main() REALLY registers (the same
// AST-enumerated live set + inert-double constructions the ACI adoption control uses), not over a fixture
// that could drift from the composition root.
//
// The RUNTIME halves live in the agent package: the loop still refuses a non-read-only tool per entry
// before any sibling is admitted or dispatched, red-proven by injecting a mutant past the registration
// gate (TestBatchWriteToolFailsClosedBeforeAnyDispatch), which is exactly the regression this enumeration
// exists to catch FIRST.
//
// KILLING MUTATIONS: flip one live tool's ReadOnly() to false — the per-tool assertion names it and
// RegisterFrom refuses it; make RegisterFrom stop refusing mutants — the refusal half reddens; empty the
// enumeration — the vacuity floor reddens (never a vacuous pass).

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/agent"
)

// invariantMutantTool is a hypothetical mutating tool — the shape a future actuation-adjacent module
// might mistakenly hand the agent's registration. It must never survive registration.
type invariantMutantTool struct{}

func (invariantMutantTool) Name() string   { return "restart-guest" }
func (invariantMutantTool) ReadOnly() bool { return false }
func (invariantMutantTool) Invoke(context.Context, map[string]string) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}

func TestEveryLiveAgentToolIsReadOnlyByConstruction(t *testing.T) {
	live := aciLiveToolFamilies(t) // rot-guarded: read out of main.go by AST, never a hand-copied list
	built := aciBuiltFamilies()

	ts := agent.NewReadOnlyToolSet()
	total := 0
	for _, fam := range live {
		tools, ok := built[fam]
		if !ok {
			t.Fatalf("main() registers agent tools from %s, but this control does not build that family — add it "+
				"to aciBuiltFamilies so the read-only invariant keeps covering the whole live set (TG-49)", fam)
		}
		for _, tl := range tools {
			total++
			// The per-tool flag IS the mechanical read-only marker the batch lane relies on.
			if !tl.ReadOnly() {
				t.Errorf("live tool %q (family %s) reports ReadOnly()=false — a mutating capability is one "+
					"registration away from the agent's CONCURRENT dispatch lane (TG-49)", tl.Name(), fam)
			}
			if err := ts.Register(tl); err != nil {
				t.Errorf("live tool %q must register into the read-only set, got %v", tl.Name(), err)
			}
		}
	}
	if total == 0 {
		// VACUITY FLOOR: an empty enumeration proves nothing.
		t.Fatal("no live tools enumerated — this invariant would hold vacuously over an empty set")
	}
	if !ts.AllReadOnly() {
		t.Fatal("the fully-registered live set must enumerate as all read-only")
	}
	// The refusal half: the gate that MAKES the invariant true must bite. A hypothetical mutating tool is
	// refused at registration and stays absent — so "all registered agent tools are read-only" is a
	// property of the construction, not a coincidence of today's modules.
	if err := ts.Register(invariantMutantTool{}); !errors.Is(err, agent.ErrWriteToolWithheld) {
		t.Fatalf("registering a mutating tool must be refused with ErrWriteToolWithheld, got %v", err)
	}
	if _, present := ts.Get("restart-guest"); present {
		t.Fatal("the refused mutating tool must be absent from the registered set")
	}
	if !ts.AllReadOnly() {
		t.Fatal("the set must remain all-read-only after a refused registration")
	}
	t.Logf("TG-49 read-only invariant: %d live agent tools enumerated, all read-only by construction; mutant registration refused", total)
}
