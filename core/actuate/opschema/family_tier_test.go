package opschema

import (
	"strings"
	"testing"
)

// TestEveryShippedOpClassDeclaresAFamilyAndTier asserts over the WHOLE registry, not a sample. A class that
// declares neither is the shape this guards against: it would join no graduation group and carry no band
// floor, so it would be governed by nothing while looking fully registered.
func TestEveryShippedOpClassDeclaresAFamilyAndTier(t *testing.T) {
	t.Parallel()
	specs := Specs()
	if len(specs) == 0 {
		t.Fatal("registry is empty — this test would pass vacuously")
	}
	for _, s := range specs {
		if !knownFamilies[s.Family] {
			t.Errorf("op-class %q declares family %q, outside the closed set", s.OpClass, s.Family)
		}
		if !knownTiers[s.SafetyTier] {
			t.Errorf("op-class %q declares safety_tier %q, outside the closed set", s.OpClass, s.SafetyTier)
		}
	}
}

// TestRegistryRejectsAnUnknownFamily — an unrecognised family silently opens a graduation ladder nobody is
// watching, so it must be fatal at init rather than merely odd.
func TestRegistryRejectsAnUnknownFamily(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("an unknown family must panic at registry build (fail closed)")
		}
		if !strings.Contains(strings.ToLower(r.(string)), "family") {
			t.Fatalf("panic did not name the family problem: %v", r)
		}
	}()
	mustBuildRegistry(
		[]byte(`{"op_classes":[{"op_class":"x-op","op":"do","family":"make-believe","safety_tier":"medium","params":[]}]}`),
		map[string]ArgvBuilder{"x-op": func(map[string]string) ([]string, error) { return []string{"true"}, nil }},
	)
}

// TestRegistryRejectsAnUnknownTier — a class with no recognised tier has no band floor.
func TestRegistryRejectsAnUnknownTier(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("an unknown safety_tier must panic at registry build (fail closed)")
		}
	}()
	mustBuildRegistry(
		[]byte(`{"op_classes":[{"op_class":"x-op","op":"do","family":"service-lifecycle","safety_tier":"mostly-fine","params":[]}]}`),
		map[string]ArgvBuilder{"x-op": func(map[string]string) ([]string, error) { return []string{"true"}, nil }},
	)
}

// TestFamilyAndTierAreNormalized — a case/whitespace variant must not dodge the closed-set check, mirroring
// how Kind() and Lookup() normalize. Otherwise `" Service-Lifecycle "` would be rejected as unknown while
// meaning exactly the known value.
func TestFamilyAndTierAreNormalized(t *testing.T) {
	t.Parallel()
	m := mustBuildRegistry(
		[]byte(`{"op_classes":[{"op_class":"x-op","op":"do","family":"  Service-Lifecycle ","safety_tier":" LOW-Reversible ","params":[]}]}`),
		map[string]ArgvBuilder{"x-op": func(map[string]string) ([]string, error) { return []string{"true"}, nil }},
	)
	got := m["x-op"]
	if got.Family != FamilyServiceLifecycle {
		t.Errorf("family = %q, want normalized %q", got.Family, FamilyServiceLifecycle)
	}
	if got.SafetyTier != TierLowReversible {
		t.Errorf("safety_tier = %q, want normalized %q", got.SafetyTier, TierLowReversible)
	}
}

// TestAutoEligibleIsSafeDirectionOnly pins the rule that irreversible and vendor-critical verbs can NEVER
// reach autonomy. Asserted over the CLOSED tier set so a newly added tier cannot default to auto-eligible by
// omission — the failure would otherwise be silent and permissive, which is the worst combination.
func TestAutoEligibleIsSafeDirectionOnly(t *testing.T) {
	t.Parallel()
	want := map[string]bool{
		TierLowReversible:  true,
		TierMedium:         true,
		TierIrreversible:   false,
		TierVendorCritical: false,
	}
	if len(want) != len(knownTiers) {
		t.Fatalf("this test enumerates %d tiers but the closed set has %d — a new tier must be classified "+
			"here explicitly, never left to default", len(want), len(knownTiers))
	}
	for tier, eligible := range want {
		if got := AutoEligible(tier); got != eligible {
			t.Errorf("AutoEligible(%q) = %v, want %v", tier, got, eligible)
		}
	}
	if AutoEligible("  IRREVERSIBLE  ") {
		t.Error("AutoEligible must normalize — a case/space variant of an ineligible tier must stay ineligible")
	}
	if AutoEligible("not-a-tier") {
		t.Error("an UNKNOWN tier must not be auto-eligible (fail closed)")
	}
}
