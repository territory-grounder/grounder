package opschema

import "testing"

// Day-zero (ADR-0016): TG with an EMPTY catalog is a full-capability shadow adviser that can execute
// nothing, and every capability must be EARNED. It is the posture the predecessor runs in by construction
// — an open-world adviser with no hand-authored catalog — so it is also the only configuration in which a
// head-to-head compares like with like.
//
// Until schemaForProfile existed the claim had NO reachable code path: an empty opschema.json panicked at
// init because all seven compiled builders became builders with no schema. The epic's central promise could
// not be exercised, and a comparison against the predecessor in that posture was impossible.

// TestDayZeroEmptyCatalogBoots is the oracle for the promise itself.
func TestDayZeroEmptyCatalogBoots(t *testing.T) {
	t.Setenv(DayZeroEnv, "1")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the day-zero profile must BOOT with an empty catalog, got panic: %v", r)
		}
	}()
	m := mustBuildRegistry(schemaForProfile(), builders)
	if len(m) != 0 {
		t.Fatalf("day-zero must compose an EMPTY registry, got %d op-class(es)", len(m))
	}
}

// TestDayZeroRegistryOffersNothingToExecute states the safety half: booting is worthless if the empty
// registry still hands out a spec. Rung 0 IS registry absence, so every lookup must come back empty —
// which is what makes "can execute nothing" true rather than merely intended.
func TestDayZeroRegistryOffersNothingToExecute(t *testing.T) {
	t.Setenv(DayZeroEnv, "1")
	m := mustBuildRegistry(schemaForProfile(), builders)
	for _, slug := range []string{"start-guest", "restart-service", "restart-container", "disk-grow"} {
		if _, ok := m[normalize(slug)]; ok {
			t.Fatalf("day-zero registry must not resolve %q — an absent class is rung 0", slug)
		}
	}
}

// TestOrphanBuilderStillPanicsWithoutTheProfile is the control that keeps the suspension HONEST.
//
// The day-zero branch skips the "compiled builder has no schema" panic. That check catches a real build
// defect — a builder someone forgot to give a schema — and suspending it unconditionally would delete a
// fail-closed guarantee from the most safety-critical file in the repo. This asserts the check still fires
// when the profile is OFF, so the suspension is scoped to exactly the posture that asked for it.
func TestOrphanBuilderStillPanicsWithoutTheProfile(t *testing.T) {
	t.Setenv(DayZeroEnv, "") // profile explicitly OFF
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("without the day-zero profile an orphaned compiled builder MUST still panic — the " +
				"fail-closed correspondence check may not be weakened for normal deployments")
		}
	}()
	_ = mustBuildRegistry([]byte(`{"op_classes":[]}`), builders)
}

// TestDayZeroIsOptInAndExact pins the profile's trigger. A deployment profile that turned on for any
// non-empty value would arm itself on "0", "false" or a stray space — and this one only ever REMOVES
// capability, so a surprise activation silently stops an estate healing.
func TestDayZeroIsOptInAndExact(t *testing.T) {
	for _, v := range []string{"", "0", "false", "yes", "true", " 1", "1 "} {
		t.Setenv(DayZeroEnv, v)
		if DayZero() {
			t.Fatalf("%q must NOT enable the day-zero profile — only an exact \"1\" may", v)
		}
	}
	t.Setenv(DayZeroEnv, "1")
	if !DayZero() {
		t.Fatal(`"1" must enable the day-zero profile`)
	}
}
