package policy

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/safety"
)

// TG-224 (port-fidelity finding #17) — the INTENT pin for the two families the destructiveness deriver
// learned in this change: network-catastrophic (interface / VLAN / route / ACL / trunk teardown) and
// code-deploy / repo-write (force-push, ref delete, deploy-key revoke, pipeline / environment destroy).
//
// THE INTENT: registering such an op-class must never be sufficient to make it auto-eligible. Autonomy for
// one of these verbs has to be an EXPLICIT floor decision — a `safety_tier` the registry author writes down
// (irreversible / vendor-critical), or the never-auto floor slug — and never something a class drifts into by
// accruing clean runs. A clean run answers "did it work?"; these verbs pose "what if it does not?", and the
// answer is a partitioned estate or a destroyed history with the audit trail inside it.
//
// WHY THESE ASSERT THROUGH Ladder.GraduatedVerdict. `opschema.AutoEligible` was previously defined,
// documented as a safety floor, unit-tested — and called by nothing, so a future irreversible verb would have
// graduated exactly as if the tier had never been declared (see tier_floor_test.go's opening note). A
// property asserted only in its own package is a property the system does not have, so every arm below drives
// the REAL decision hook the engine calls.

// destructiveFamilies are the op-class families whose verbs, if the catalogue ever gains one, can partition
// the estate or destroy code history. `network-device` is already in opschema's closed family set (its own
// comment: "vendor gear, can PARTITION the estate"); repo-write verbs have no family yet, which is precisely
// why the second arm below substitutes one rather than waiting for the catalogue to land it.
var destructiveFamilies = map[string]bool{
	opschema.FamilyNetworkDevice: true,
}

// TestNoRegisteredClassInADestructiveFamilyIsAutoEligible walks the LIVE registry. Today the catalogue ships
// no network-device or repo-write class, so this arm passes by finding nothing — and says so out loud instead
// of pretending to be coverage. The substitution arms below are what make the property provable NOW, before
// the first such class lands, which is the only moment at which knowing these verbs helps.
func TestNoRegisteredClassInADestructiveFamilyIsAutoEligible(t *testing.T) {
	ctx := context.Background()
	specs := opschema.Specs()
	if len(specs) == 0 {
		t.Fatal("registry is empty — this test would pass vacuously")
	}
	checked := 0
	for _, s := range specs {
		blob := strings.Join(append([]string{s.OpClass, s.Op}, s.ArgvTemplate...), " ")
		serverDestructive := safety.IsDestructiveOp(blob)
		if !destructiveFamilies[s.Family] && !serverDestructive && !safety.IsNeverAuto(s.OpClass) {
			continue
		}
		checked++
		if opschema.AutoEligible(s.SafetyTier) {
			t.Errorf("op-class %q (family %s, tier %s) is server-derived destructive or in a destructive "+
				"family, yet its declared tier is auto-eligible — registering the verb is not a floor decision, "+
				"and this one was made by omission", s.OpClass, s.Family, s.SafetyTier)
		}
		// …and through the REAL hook, after enough clean runs to promote anything promotable.
		l := NewLadder(1, NewMemGraduationStore(), nil)
		if _, err := l.Record(ctx, s.OpClass, OutcomeVerifiedClean); err != nil {
			t.Fatalf("%s: %v", s.OpClass, err)
		}
		if got := l.GraduatedVerdict(ctx, s.OpClass, VerdictAuto); got == VerdictAuto {
			t.Errorf("op-class %q reached auto at the decision point despite being destructive — the floor is "+
				"bypassed", s.OpClass)
		}
	}
	t.Logf("live registry: %d of %d op-classes are in a destructive family or server-derived destructive "+
		"(0 is expected today — registry-only argv means these verbs cannot execute; the substitution arms "+
		"below are what prove the floor before the catalogue lands one)", checked, len(specs))
}

// TestADestructiveVerbNeedsAnExplicitFloorDecisionToBeRefused is the substitution arm, and it is deliberately
// TWO-SIDED. The permissive side is not a mistake: it states the finding's actual claim. Registration alone
// does NOT refuse these verbs — a network-teardown class registered with an auto-eligible tier graduates to
// `auto` like any restart, because the ladder counts clean runs and nothing else. What refuses it is the
// explicit tier the registry author writes. A one-sided test would show the floor working and hide the fact
// that the floor is the ONLY thing between a registered `write-erase` and autonomy.
func TestADestructiveVerbNeedsAnExplicitFloorDecisionToBeRefused(t *testing.T) {
	ctx := context.Background()

	// (1) The permissive side: an auto-ELIGIBLE tier lets a network-teardown verb graduate. This is the hazard
	// the floor exists for, demonstrated rather than asserted away.
	withTiers(t, map[string]string{"write-erase": opschema.TierLowReversible})
	l := NewLadder(1, NewMemGraduationStore(), nil)
	if _, err := l.Record(ctx, "write-erase", OutcomeVerifiedClean); err != nil {
		t.Fatal(err)
	}
	if got := l.GraduatedVerdict(ctx, "write-erase", VerdictAuto); got != VerdictAuto {
		t.Fatalf("a class declared low-reversible got %v — the ladder must be shown to promote on tier alone, "+
			"or the next assertion proves nothing about the tier", got)
	}

	// (2) The floor decision: the SAME class with an honest tier is refused at the decision point, no matter
	// how many clean runs it accrues.
	for _, tier := range []string{opschema.TierIrreversible, opschema.TierVendorCritical} {
		withTiers(t, map[string]string{"write-erase": tier})
		st := NewMemGraduationStore()
		if err := st.Save(ctx, ClassState{OpClass: "write-erase", Level: LevelAuto}); err != nil {
			t.Fatal(err)
		}
		// Seeded straight to LevelAuto on purpose: the promotion guard would otherwise refuse to promote it and
		// the two guards would mask each other, leaving the DECISION floor unproven.
		l := NewLadder(1, st, nil)
		if got := l.GraduatedVerdict(ctx, "write-erase", VerdictAuto); got != VerdictApprove {
			t.Errorf("write-erase at tier %s is seeded to LevelAuto and got %v — an auto-forbidding tier must "+
				"still be refused at the decision point", tier, got)
		}
	}

	// (3) The other route to the same refusal: the never-auto floor SLUG. It is what catches a verb that
	// arrives as a DECLARED op_class rather than as a raw command, and it is non-configurable — no tier, no
	// ladder state and no policy can lift it.
	for _, op := range []string{"write-erase", "no-ip-routing", "force-push", "branch-delete", "deploy-key-revoke"} {
		if !safety.IsNeverAuto(op) {
			t.Errorf("op-class %q is not on the never-auto floor — the slug list is the route a DECLARED "+
				"class takes to the floor, and it must be populated before the class is", op)
		}
	}
}

// TestAnUnregisteredDestructiveVerbIsNeverPermitted — defence in depth, and the state TG is actually in
// today. These verbs are not in the catalogue at all, so there is no builder and no argv template for them:
// they cannot execute by construction. That must read as REFUSAL at the decision point, never as silence.
func TestAnUnregisteredDestructiveVerbIsNeverPermitted(t *testing.T) {
	ctx := context.Background()
	for _, op := range []string{"write-erase", "no-ip-routing", "force-push", "pipeline-delete"} {
		if _, registered := opschema.Lookup(op); registered {
			t.Fatalf("%q is now REGISTERED — this test's premise changed, which is the forcing function: give "+
				"it an explicit safety_tier and confirm the floor arms above still refuse it", op)
		}
		st := NewMemGraduationStore()
		if err := st.Save(ctx, ClassState{OpClass: op, Level: LevelAuto}); err != nil {
			t.Fatal(err)
		}
		if got := NewLadder(1, st, nil).GraduatedVerdict(ctx, op, VerdictAuto); got != VerdictApprove {
			t.Errorf("unregistered op-class %q seeded to auto got %v, want approve — a missing declaration must "+
				"never read as permission", op, got)
		}
	}
}
