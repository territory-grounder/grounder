package opschema

import (
	"encoding/json"
	"strings"
	"testing"
)

// T-029-1 (spec/029 REQ-2904; sign-off TG-488 B5): commit-confirmed eligibility is DATA with a
// conservative validated floor. These drills execute every refusal the rules promise — including the
// dangling-inverse registry panic and the omitempty byte-stability that keeps ratified overlay
// hashes unchanged.

func ccTestSpec(mut func(*OpClassSpec)) OpClassSpec {
	s := OpClassSpec{
		OpClass: "cc-drill-restart", Op: "restart", Family: "service-lifecycle",
		SafetyTier: "low-reversible",
		Params:     []ParamSpec{{Name: "unit", Type: "string", Required: true}},
		ArgvTemplate:     []string{"systemctl", "restart", "${unit}"},
		RollbackTemplate: []string{"systemctl", "restart", "${unit}"},
		CommitConfirmed:  &CommitConfirmedSpec{WindowSeconds: 600},
	}
	if mut != nil {
		mut(&s)
	}
	return s
}

func TestCommitConfirmedValidDeclarationPasses(t *testing.T) {
	if _, err := ValidateSpec(ccTestSpec(nil), false); err != nil {
		t.Fatalf("a well-formed commit_confirmed declaration must validate: %v", err)
	}
}

func TestCommitConfirmedWithoutAnyInverseIsRefused(t *testing.T) {
	_, err := ValidateSpec(ccTestSpec(func(s *OpClassSpec) { s.RollbackTemplate = nil }), false)
	if err == nil || !strings.Contains(err.Error(), "REQ-2904") {
		t.Fatalf("eligibility without an inverse must refuse naming REQ-2904, got %v", err)
	}
}

func TestCommitConfirmedWithBothInversesIsRefused(t *testing.T) {
	_, err := ValidateSpec(ccTestSpec(func(s *OpClassSpec) { s.RollbackOpClass = "stop-something" }), false)
	if err == nil || !strings.Contains(err.Error(), "EXACTLY ONE") {
		t.Fatalf("two inverse declarations must refuse, got %v", err)
	}
}

func TestCommitConfirmedWindowUnderTheFloorIsRefused(t *testing.T) {
	_, err := ValidateSpec(ccTestSpec(func(s *OpClassSpec) { s.CommitConfirmed.WindowSeconds = 299 }), false)
	if err == nil || !strings.Contains(err.Error(), "floor") {
		t.Fatalf("a sub-floor window must refuse, got %v", err)
	}
}

func TestCommitConfirmedAwxWindowMustExceedTheDeferredVerifyBound(t *testing.T) {
	awx := func(w int) OpClassSpec {
		return ccTestSpec(func(s *OpClassSpec) {
			s.OpClass, s.Family, s.SafetyTier = "cc-drill-awx", "resource-resize", "medium"
			s.EffectKind = EffectAWXLaunch
			s.ArgvTemplate = nil
			s.CommitConfirmed.WindowSeconds = w
		})
	}
	if _, err := ValidateSpec(awx(AwxWindowFloorSeconds), false); err == nil ||
		!strings.Contains(err.Error(), "deferred verify") {
		t.Fatalf("an awx window at the bound must refuse (verdict may still be in flight), got %v", err)
	}
	if _, err := ValidateSpec(awx(AwxWindowFloorSeconds+1), false); err != nil {
		t.Fatalf("an awx window past the bound must validate: %v", err)
	}
}

func TestRollbackOpClassWithoutCommitConfirmedIsRefused(t *testing.T) {
	_, err := ValidateSpec(ccTestSpec(func(s *OpClassSpec) {
		s.CommitConfirmed = nil
		s.RollbackTemplate = nil
		s.RollbackOpClass = "stop-something"
	}), false)
	if err == nil || !strings.Contains(err.Error(), "dangling") {
		t.Fatalf("a dangling inverse declaration must refuse, got %v", err)
	}
}

// KILLING MUTATION — the registry cross-reference: a rollback_op_class naming a class that does not
// exist refuses the WHOLE registry at load (panic — the embedded registry is compiled in, and dying
// at boot beats arming a revert that dies at fire time).
func TestDanglingRollbackOpClassRefusesTheRegistry(t *testing.T) {
	schema := `{"op_classes":[
	  {"op_class":"cc-drill-start","op":"start","family":"service-lifecycle","safety_tier":"low-reversible",
	   "params":[{"name":"unit","required":true,"type":"string"}],
	   "argv_template":["systemctl","start","${unit}"],
	   "rollback_op_class":"cc-drill-stop-DOES-NOT-EXIST",
	   "commit_confirmed":{"window_seconds":600}}]}`
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a dangling rollback_op_class must refuse the registry (no panic happened)")
		}
		if !strings.Contains(r.(string), "does not exist") {
			t.Fatalf("the refusal must name the dangling class, got %v", r)
		}
	}()
	mustBuildRegistry([]byte(schema), nil)
}

// The omitempty stability guarantee: a spec WITHOUT the T-029-1 fields serializes to bytes that do
// not mention them, so every pre-029 overlay CanonicalHash is unchanged (the requires_target_state
// precedent, re-proven for these fields).
func TestCommitConfirmedAbsenceIsByteInvisible(t *testing.T) {
	s := ccTestSpec(func(s *OpClassSpec) { s.CommitConfirmed = nil; s.RollbackTemplate = nil; s.RollbackOpClass = "" })
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"commit_confirmed", "rollback_op_class"} {
		if strings.Contains(string(b), field) {
			t.Fatalf("unset %s must not appear in the serialization (CanonicalHash stability): %s", field, b)
		}
	}
}

// The embedded registry itself must load with the v1 declarations (restart/reload) — and start-guest
// must remain NOT eligible until its stop inverse exists in the registry (the sign-off's "inverse:
// stop" enters with T-029-3/TG-464, refused-by-construction until then).
func TestEmbeddedRegistryCarriesTheV1Declarations(t *testing.T) {
	for _, name := range []string{"restart-service", "reload-service"} {
		s, ok := Lookup(name)
		if !ok {
			t.Fatalf("%s missing from the embedded registry", name)
		}
		if s.CommitConfirmed == nil || s.CommitConfirmed.WindowSeconds < commitConfirmWindowFloorSeconds {
			t.Fatalf("%s must carry a valid commit_confirmed declaration, got %+v", name, s.CommitConfirmed)
		}
		if len(s.RollbackTemplate) == 0 {
			t.Fatalf("%s must declare its explicit self-inverse rollback_template", name)
		}
	}
	// T-029-3 delivered the stop inverse, flipping T-029-1's refused-by-construction pin to the
	// ruled v1 state (TG-488 B5: start-guest eligible, inverse stop): eligibility + the resolving
	// class inverse + the blind-stop precondition all present, or the ruling is not implemented.
	s, ok := Lookup("start-guest")
	if !ok || s.CommitConfirmed == nil || s.CommitConfirmed.WindowSeconds < commitConfirmWindowFloorSeconds {
		t.Fatalf("start-guest must be commit-confirmed eligible now that its stop inverse exists (got ok=%v cc=%+v)", ok, s.CommitConfirmed)
	}
	if s.RollbackOpClass != "stop-guest" {
		t.Fatalf("start-guest's declared inverse must be the stop-guest CLASS (lifecycle classes invert by class, not argv), got %q", s.RollbackOpClass)
	}
	inv, ok := Lookup("stop-guest")
	if !ok {
		t.Fatal("stop-guest must exist as a registered, compiled class — a dangling inverse would die at fire time")
	}
	if inv.RequiresTargetState != RequiresRunning {
		t.Fatalf("stop-guest must declare requires_target_state=running (the blind-stop guard), got %q", inv.RequiresTargetState)
	}
	if inv.CommitConfirmed != nil {
		t.Fatalf("stop-guest is inverse-only and must NOT itself be commit-confirmed eligible (no revert-of-revert chains), got %+v", inv.CommitConfirmed)
	}
}

// spec/029 T-029-3, gate-caught: an INVERSE-ONLY class (a declared rollback_op_class target) is
// actuatable but must NEVER render in the proposal catalog — the model is never OFFERED the stop
// verb (the opcover exemption's own rationale), and the change gate measured the judged
// dimensions drop when stop-guest's entry + registry meta-prose entered every preamble.
//
// KILLING MUTATION (executed 2026-08-14): in Catalog(), drop the inverseOnly skip — stop-guest
// re-enters the render and this drill goes red on both halves. Restored, green.
func TestCatalogNeverOffersInverseOnlyClasses(t *testing.T) {
	cat := Catalog()
	if cat == "" {
		t.Fatal("vacuity floor: the embedded registry must render a non-empty catalog")
	}
	if !strings.Contains(cat, "- start-guest") {
		t.Fatal("the forward class start-guest must render (the exclusion must not over-reach)")
	}
	if strings.Contains(cat, "stop-guest") {
		t.Fatal("stop-guest is inverse-only and must NOT be offered to the model — registering it made it " +
			"actuatable (the fired revert needs that); rendering it made it proposable (nothing may)")
	}
}
