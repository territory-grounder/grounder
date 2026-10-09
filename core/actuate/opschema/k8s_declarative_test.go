package opschema

import "testing"

// TG-122 slice 3 — the k8s-declarative effect kind is a VALID, launch-encoded (no compiled builder) kind.
// Mirrors the awx-launch admission rules: accepted without a builder, refused WITH one, and the unknown-kind
// error names it.
func TestK8sDeclarativeEffectKindAdmission(t *testing.T) {
	base := OpClassSpec{
		OpClass:    "k8s-set-replicas",
		Op:         "set replicas",
		Family:     FamilyServiceLifecycle,
		SafetyTier: TierLowReversible,
		EffectKind: EffectK8sDeclarative,
		Params:     []ParamSpec{{Name: "replicas", Required: true}},
	}

	// Accepted as a launch-encoded kind with NO compiled builder.
	if _, err := ValidateSpec(base, false); err != nil {
		t.Fatalf("a k8s-declarative op-class with no builder must validate: %v", err)
	}
	// It is NOT argv-encoded, so Argv must refuse it (its effect is encoded for the gitops-mr channel).
	spec, _ := ValidateSpec(base, false)
	if argvEncoded(spec.Kind()) {
		t.Fatal("k8s-declarative must NOT be argv-encoded — its effect is a gitops-mr ProposeSpec, not an argv")
	}

	// A compiled builder is a contradiction (the runner encodes the ProposeSpec, like awx-launch).
	// KILLING MUTATION: drop the `case EffectK8sDeclarative` builder guard → this passes and reddens.
	if _, err := ValidateSpec(base, true); err == nil {
		t.Fatal("a k8s-declarative op-class WITH a compiled builder must be refused (two definitions of the effect)")
	}

	// The unknown-kind path still fails closed and now enumerates k8s-declarative among the known kinds.
	bad := base
	bad.EffectKind = "made-up-channel"
	if _, err := ValidateSpec(bad, false); err == nil {
		t.Fatal("an unknown effect_kind must fail closed")
	}
}
