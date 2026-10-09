package trace

import (
	"os"
	"reflect"
	"regexp"
	"testing"
)

// REQ-2001 — "one observe-only trace step at each decision boundary that already runs … WHERE each emit
// neither blocks nor alters the boundary it observes, and the emitter SHALL degrade to a no-op rather
// than fail the decision path when the trace sink is absent."
//
// WHAT THESE ORACLES DO AND DO NOT CLAIM. They do NOT close REQ-2001's acceptance scenario, which stays
// @pending. As of TG-412 all SEVEN boundaries the requirement names — classify, each interceptor gate,
// each ReAct cycle, policy Decide, credential Resolve, regime select, verify — ARE representable: regime
// select now has its own StepKind (StepRegime), its SpineRecords slot (Regime RegimeRecord) and its
// Assemble arm. What still holds the scenario open is a wording reconciliation, not a missing boundary:
// REQ-2001 describes an "emit ... nil-safe side-write" while the tracer DERIVES — each boundary already
// leaves a durable row and the pure Assemble reconstructs the trace from the Present flags. Closing the
// scenario means reconciling that emit-vs-derive language in the spec (spec/007 lockstep), separate work.
//
// What they DO pin is the part that holds, and the part most likely to rot silently: that the declared
// step vocabulary and the assembler cannot drift apart, and that an absent boundary degrades to nothing
// rather than to a failure. The tracer DERIVES rather than emits — every boundary already leaves a
// durable row and the pure Assemble reconstructs the trace from the Present flags — so "degrades to a
// no-op" here means an absent sub-record contributes no step and cannot fail the read.

var stepKindDecl = regexp.MustCompile(`(?m)^\s*(Step[A-Za-z]+)\s+StepKind\s*=\s*"([^"]+)"`)

// THE CLOSED ENUMERATION. A StepKind declared in record.go and never assembled is a boundary that exists
// in the vocabulary and produces nothing — the shape this repo keeps rediscovering, where a capability is
// present in the tree and absent from the output. A source scan is used deliberately rather than a
// hand-written list of kinds, because a hand-written list is a second copy of the enum and would go stale
// in exactly the case this guard exists to catch: someone adding the next kind.
func TestEveryDeclaredStepKindIsReachedByTheAssembler(t *testing.T) {
	decl, err := os.ReadFile("record.go")
	if err != nil {
		t.Fatalf("read record.go: %v", err)
	}
	asm, err := os.ReadFile("assemble.go")
	if err != nil {
		t.Fatalf("read assemble.go: %v", err)
	}

	kinds := stepKindDecl.FindAllStringSubmatch(string(decl), -1)
	// ANTI-VACUITY. If the pattern stops matching — a rename, a reformat, a move to another file — this
	// test would iterate an empty set and pass while checking nothing at all.
	if len(kinds) < 14 {
		t.Fatalf("parsed only %d StepKind declarations from record.go; the enum had 14 when this guard was "+
			"written (13 originally, +StepRegime in TG-412), so the pattern has stopped matching and every "+
			"assertion below is vacuous", len(kinds))
	}

	for _, k := range kinds {
		name := k[1]
		if !regexp.MustCompile(`\b` + name + `\b`).Match(asm) {
			t.Errorf("%s (%q) is declared in record.go and never referenced in assemble.go — a step kind "+
				"nothing produces is a decision boundary the trace silently omits", name, k[2])
		}
	}
}

// THE NO-OP HALF OF REQ-2001. Nothing is present, so nothing is traced — and the read must not fail.
func TestAnAbsentBoundaryContributesNoStepAndDoesNotFail(t *testing.T) {
	got := Assemble("ext-ref-absent", SpineRecords{})

	if got.Steps == nil {
		t.Error("Steps must be an empty slice, not nil — a nil slice and an absent trace render " +
			"identically to a caller that only checks length, and JSON-encodes as null rather than []")
	}
	if len(got.Steps) != 0 {
		t.Errorf("a spine with no present boundary must yield no steps, got %d: %+v", len(got.Steps), got.Steps)
	}
	if got.ExternalRef != "ext-ref-absent" {
		t.Errorf("the trace must still identify itself, got %q", got.ExternalRef)
	}
}

// OBSERVE-ONLY, in the shape this design gives it: the assembler may not alter what it observes. The
// boundaries are already finished by the time Assemble runs, so the property that carries the
// requirement's intent here is that assembly is pure with respect to its input.
func TestAssembleDoesNotMutateTheRecordsItObserves(t *testing.T) {
	in := SpineRecords{}
	in.Classification.Present = true
	in.Classification.Band = "AUTO"
	in.Triage.Present = true
	in.Triage.Host = "web01"
	before := in

	_ = Assemble("ext-ref-purity", in)

	if !reflect.DeepEqual(in, before) {
		t.Errorf("Assemble mutated its input — the tracer must observe the decision path without altering "+
			"it (REQ-2001/REQ-2002).\n before: %+v\n  after: %+v", before, in)
	}
}

// A PRESENT boundary must actually produce a step, or the two oracles above both pass over an assembler
// that yields nothing for everything.
func TestAPresentBoundaryProducesAStep(t *testing.T) {
	in := SpineRecords{}
	in.Classification.Present = true
	in.Classification.Band = "AUTO"

	got := Assemble("ext-ref-present", in)

	if len(got.Steps) == 0 {
		t.Fatal("a present classification boundary produced NO step — with this failing, " +
			"TestAnAbsentBoundaryContributesNoStepAndDoesNotFail is satisfied by an assembler that has " +
			"simply stopped working")
	}
	var sawClassify bool
	for _, s := range got.Steps {
		if s.Kind == StepClassify {
			sawClassify = true
		}
	}
	if !sawClassify {
		t.Errorf("a present classification must yield a %q step, got kinds %v", StepClassify, kindsOfTraceSteps(got.Steps))
	}
}

func kindsOfTraceSteps(steps []Step) []StepKind {
	out := make([]StepKind, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Kind)
	}
	return out
}
