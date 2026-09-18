package policy

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
)

// This file exists because the tier floor shipped UNREACHABLE. opschema.AutoEligible was defined, documented
// as a safety floor and unit-tested — and nothing in the repo called it. A future `irreversible` verb would
// have graduated to AUTO exactly as if the tier had never been declared. A property that is only asserted in
// its own package is a property the system does not have.
//
// These tests assert over the LIVE registry and through the REAL decision path, so the floor cannot become
// decorative again without one of them failing.

// withTiers substitutes the op-class→tier resolver for one test. Necessary because EVERY class shipped today
// is auto-eligible: without a class the floor can actually bite on, the floor is unprovable, and a test that
// passes whether or not the guard is wired is exactly how this property shipped unreachable.
func withTiers(t *testing.T, tiers map[string]string) {
	t.Helper()
	prev := opClassTier
	opClassTier = func(op string) (string, bool) {
		tier, ok := tiers[op]
		return tier, ok
	}
	t.Cleanup(func() { opClassTier = prev })
}

// TestTierFloorIsWiredIntoTheDecisionPath is the reachability oracle: it drives GraduatedVerdict — the actual
// hook the engine calls — with a REGISTERED but auto-forbidding class, which is the only input that can tell
// a wired floor apart from an unwired one.
func TestTierFloorIsWiredIntoTheDecisionPath(t *testing.T) {
	ctx := context.Background()
	withTiers(t, map[string]string{
		"reversible-verb":  opschema.TierLowReversible,
		"destructive-verb": opschema.TierIrreversible,
		"vendor-verb":      opschema.TierVendorCritical,
	})

	// SEED the durable state straight to LevelAuto rather than earning it. This is the only input that
	// isolates the DECISION floor: the promotion guard would otherwise refuse to promote these classes, so a
	// test that earns its way up passes whether or not the decision floor is wired — the two guards mask each
	// other, and a control that cannot fail is not a control.
	for _, op := range []string{"destructive-verb", "vendor-verb"} {
		st := NewMemGraduationStore()
		if err := st.Save(ctx, ClassState{OpClass: op, Level: LevelAuto}); err != nil {
			t.Fatal(err)
		}
		l := NewLadder(1, st, nil)
		if got := l.GraduatedVerdict(ctx, op, VerdictAuto); got != VerdictApprove {
			t.Errorf("%s is at LevelAuto in the store and got %v — an auto-forbidding tier must still be "+
				"refused at the decision point", op, got)
		}
	}

	// Same isolation for the UNREGISTERED case: seed it to auto, then the floor must still refuse.
	{
		st := NewMemGraduationStore()
		if err := st.Save(ctx, ClassState{OpClass: "not-a-registered-verb", Level: LevelAuto}); err != nil {
			t.Fatal(err)
		}
		l := NewLadder(1, st, nil)
		if got := l.GraduatedVerdict(ctx, "not-a-registered-verb", VerdictAuto); got != VerdictApprove {
			t.Errorf("unregistered op-class seeded to auto got %v, want approve — a missing declaration "+
				"must never read as permission", got)
		}
	}

	// An auto-ELIGIBLE class must still graduate — the floor must not blanket-deny.
	l := NewLadder(1, NewMemGraduationStore(), nil)
	if _, err := l.Record(ctx, "reversible-verb", OutcomeVerifiedClean); err != nil {
		t.Fatal(err)
	}
	if got := l.GraduatedVerdict(ctx, "reversible-verb", VerdictAuto); got != VerdictAuto {
		t.Errorf("an auto-eligible class that earned auto got %v, want auto — the floor must not over-clamp", got)
	}

}

// TestEveryRegisteredClassAgreesWithItsTier walks the WHOLE live registry and drives each class to the
// promote threshold. A class whose tier is not auto-eligible must NEVER be handed auto, no matter how many
// clean runs it accrues — which is the entire point of a tier as opposed to a counter.
func TestEveryRegisteredClassAgreesWithItsTier(t *testing.T) {
	ctx := context.Background()
	specs := opschema.Specs()
	if len(specs) == 0 {
		t.Fatal("registry is empty — this test would pass vacuously")
	}
	for _, s := range specs {
		l := NewLadder(1, NewMemGraduationStore(), nil)
		// one clean run at threshold 1 is enough to promote anything the ladder is willing to promote
		if _, err := l.Record(ctx, s.OpClass, OutcomeVerifiedClean); err != nil {
			t.Fatalf("%s: %v", s.OpClass, err)
		}
		got := l.GraduatedVerdict(ctx, s.OpClass, VerdictAuto)
		wantAuto := opschema.AutoEligible(s.SafetyTier)
		if wantAuto && got != VerdictAuto {
			t.Errorf("%s (tier %s) is auto-eligible but got %v after a clean run", s.OpClass, s.SafetyTier, got)
		}
		if !wantAuto && got == VerdictAuto {
			t.Errorf("%s (tier %s) is NOT auto-eligible but reached auto — the tier floor is bypassed",
				s.OpClass, s.SafetyTier)
		}
	}
}

// TestIneligibleTierNeverAccumulatesAnAutoRow — defence in depth. Even the durable ladder row must not read
// `auto` for a class whose tier forbids it, or an operator inspecting policy_graduation would believe a class
// is autonomous when every decision will in fact route it to a human.
func TestIneligibleTierNeverAccumulatesAnAutoRow(t *testing.T) {
	ctx := context.Background()
	// Find a registered class whose tier forbids auto. If none exists yet the test states that plainly
	// rather than passing silently — this guard becomes load-bearing the moment the catalogue lands its
	// first irreversible or vendor-critical verb.
	var ineligible string
	for _, s := range opschema.Specs() {
		if !opschema.AutoEligible(s.SafetyTier) {
			ineligible = s.OpClass
			break
		}
	}
	if ineligible == "" {
		// No SHIPPED class forbids auto yet, so substitute one — the durable-row guard must be provable
		// before the catalogue lands its first irreversible verb, not after.
		withTiers(t, map[string]string{"destructive-verb": opschema.TierIrreversible})
		ineligible = "destructive-verb"
	}
	l := NewLadder(1, NewMemGraduationStore(), nil)
	for i := 0; i < 5; i++ {
		if _, err := l.Record(ctx, ineligible, OutcomeVerifiedClean); err != nil {
			t.Fatal(err)
		}
	}
	if lvl := l.LevelOf(ctx, ineligible); lvl == LevelAuto {
		t.Fatalf("%s reached LevelAuto in the durable ladder despite an auto-forbidding tier", ineligible)
	}
}
