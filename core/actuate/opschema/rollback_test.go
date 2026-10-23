package opschema

import (
	"reflect"
	"strings"
	"testing"
)

// A rollback is the ONE argv that runs when something has already gone wrong, so it is the argv least likely
// to be exercised and most expensive to get wrong. Declaring it in the registry beside the forward — under the
// same validation — is what stops the pair drifting; these tests are that claim.

// TestRollbackDefaultsToARerunOfTheForward — INV-07 requires a BOUND rollback, not a perfect one. For the
// idempotent verbs, re-running reconverges to the known-good state, so an absent declaration is correct rather
// than missing.
func TestRollbackDefaultsToARerunOfTheForward(t *testing.T) {
	t.Parallel()
	spec := OpClassSpec{OpClass: "t", Family: FamilyServiceLifecycle, SafetyTier: TierLowReversible,
		ArgvTemplate: []string{"systemctl", "restart", "${unit}"},
		Params:       []ParamSpec{{Name: "unit", Type: "string", Required: true}}}
	params := map[string]string{"unit": "nginx"}
	fwd, err := spec.Argv(params)
	if err != nil {
		t.Fatal(err)
	}
	back, err := spec.RollbackArgv(params)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fwd, back) {
		t.Errorf("with no declared rollback, the compensating action is %v, want a re-run of %v", back, fwd)
	}
}

// TestADeclaredRollbackIsRenderedNotTheForward — the case the default is wrong for. A start's compensating
// action is a stop; re-running the start would record a rollback that compensated for nothing.
func TestADeclaredRollbackIsRenderedNotTheForward(t *testing.T) {
	t.Parallel()
	spec := OpClassSpec{OpClass: "t", Family: FamilyServiceLifecycle, SafetyTier: TierLowReversible,
		ArgvTemplate:     []string{"systemctl", "start", "${unit}"},
		RollbackTemplate: []string{"systemctl", "stop", "${unit}"},
		Params:           []ParamSpec{{Name: "unit", Type: "string", Required: true}}}
	back, err := spec.RollbackArgv(map[string]string{"unit": "nginx"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"systemctl", "stop", "nginx"}; !reflect.DeepEqual(back, want) {
		t.Errorf("rollback rendered %v, want %v", back, want)
	}
}

// TestADeclaredRollbackTemplateIsValidatedLikeTheForward is the important one. A rollback carrying an
// undeclared slot, an optional slot, or a slot embedded in a larger element would fail exactly as the forward
// would — except later, in the recovery path, where nobody is watching. It must fail closed at BOOT.
func TestADeclaredRollbackTemplateIsValidatedLikeTheForward(t *testing.T) {
	for name, rollback := range map[string]string{
		"undeclared slot": `["systemctl","stop","${nope}"]`,
		"embedded slot":   `["systemctl","stop","--unit=${unit}"]`,
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("a rollback_template with an %s was ACCEPTED at registry build — it would fail in "+
						"the recovery path instead, which is the worst place to discover it", name)
				}
			}()
			mustBuildRegistry([]byte(`{"op_classes":[{"op_class":"t","op":"do","family":"service-lifecycle",
				"safety_tier":"low-reversible","argv_template":["systemctl","start","${unit}"],
				"rollback_template":`+rollback+`,
				"params":[{"name":"unit","type":"string","required":true}]}]}`), map[string]ArgvBuilder{})
		})
	}

	// an OPTIONAL slot in a rollback is the same silent-wrong-command hazard as in a forward
	t.Run("optional slot", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("a rollback_template slot on a non-REQUIRED param was accepted — it would render BLANK")
			} else if !strings.Contains(r.(string), "REQUIRED") {
				t.Errorf("panic did not name the problem: %v", r)
			}
		}()
		mustBuildRegistry([]byte(`{"op_classes":[{"op_class":"t","op":"do","family":"service-lifecycle",
			"safety_tier":"low-reversible","argv_template":["systemctl","start","x"],
			"rollback_template":["systemctl","stop","${unit}"],
			"params":[{"name":"unit","type":"string","required":false}]}]}`), map[string]ArgvBuilder{})
	})
}

// TestEveryShippedNonIdempotentVerbDeclaresItsInverse walks the LIVE registry. A verb whose op is `start` and
// whose rollback is a re-run is a verb that cannot be rolled back, and the ledger would say otherwise.
func TestEveryShippedNonIdempotentVerbDeclaresItsInverse(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, s := range Specs() {
		if s.Op != "start" || len(s.ArgvTemplate) == 0 {
			continue
		}
		checked++
		if len(s.RollbackTemplate) == 0 {
			t.Errorf("%s is a `start` verb with NO declared rollback — its compensating action would default "+
				"to another start, which undoes nothing while the ledger records a rollback", s.OpClass)
			continue
		}
		if reflect.DeepEqual(s.RollbackTemplate, s.ArgvTemplate) {
			t.Errorf("%s declares a rollback identical to its forward", s.OpClass)
		}
	}
	if checked == 0 {
		t.Skip("no templated `start` verb in the registry")
	}
}
