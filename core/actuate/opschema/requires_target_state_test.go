package opschema

// TG-378: requires_target_state is a CLOSED per-class state-precondition declaration. These oracles pin
// (1) the registry actually declares it on start-guest (the pve03 class), (2) the validation refuses an
// unknown value — an unrecognised precondition would read as "declares nothing" to a gate matching known
// values and silently never fire, and (3) the omitempty promise: a spec that does not use the vocabulary
// hashes byte-identically to its pre-TG-378 self, so no ratified overlay entry_hash moves.
//
// KILLING MUTATION (executed 2026-08-11): remove the closed-set check in ValidateSpec —
// TestRequiresTargetStateIsAClosedEnum fails on the "powered-down" imposter. Restore → green.

import "testing"

func TestStartGuestDeclaresItsStatePrecondition(t *testing.T) {
	spec, ok := Lookup("start-guest")
	if !ok {
		t.Fatal("start-guest missing from the registry")
	}
	if spec.RequiresTargetState != RequiresNotRunning {
		t.Fatalf("start-guest must declare requires_target_state=%q (the pve03 class), got %q",
			RequiresNotRunning, spec.RequiresTargetState)
	}
}

func TestRequiresTargetStateIsAClosedEnum(t *testing.T) {
	base := OpClassSpec{OpClass: "x-test", Op: "start", Family: "guest-lifecycle", SafetyTier: "low-reversible",
		EffectKind: "proxmox-lifecycle", ArgvTemplate: []string{"start", "${guest}"},
		Params: []ParamSpec{{Name: "guest", Type: "string", Required: true}}}

	base.RequiresTargetState = "powered-down" // an imposter value
	if _, err := ValidateSpec(base, false); err == nil {
		t.Fatal("an unknown requires_target_state must be REFUSED — it would silently never be enforced")
	}
	base.RequiresTargetState = "Not-Running" // case/space normalisation must land on the closed value
	got, err := ValidateSpec(base, false)
	if err != nil || got.RequiresTargetState != RequiresNotRunning {
		t.Fatalf("normalisation must accept the closed value: %v / %q", err, got.RequiresTargetState)
	}
	base.RequiresTargetState = ""
	if _, err := ValidateSpec(base, false); err != nil {
		t.Fatalf("empty (no precondition) must remain valid: %v", err)
	}
}

// TestOmitemptyKeepsUndeclaredSpecsHashStable: the vocabulary addition must not move any existing
// overlay's CanonicalHash — a ratified entry_hash that shifts under a field the spec does not use would
// invalidate the opclass:ratify ledger records.
func TestOmitemptyKeepsUndeclaredSpecsHashStable(t *testing.T) {
	s := OpClassSpec{OpClass: "x-hash", Op: "restart", Family: "service-reconverge", SafetyTier: "low-reversible",
		ArgvTemplate: []string{"systemctl", "restart", "${unit}"},
		Params:       []ParamSpec{{Name: "unit", Type: "string", Required: true}}}
	h1, err := CanonicalHash(s)
	if err != nil {
		t.Fatal(err)
	}
	// The zero value of the new field must be invisible to the hash (omitempty).
	s.RequiresTargetState = ""
	h2, err := CanonicalHash(s)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("an empty requires_target_state moved the canonical hash (%s -> %s) — every ratified overlay entry_hash would shift", h1, h2)
	}
	// And a DECLARED value must move it (the hash must see the field when it means something).
	s.RequiresTargetState = RequiresNotRunning
	h3, err := CanonicalHash(s)
	if err != nil {
		t.Fatal(err)
	}
	if h3 == h1 {
		t.Fatal("a declared requires_target_state did NOT move the canonical hash — the ratification record would not cover the precondition")
	}
}