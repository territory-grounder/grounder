package runner

// TG-122 slice 3 — the k8s-declarative effect kind routes to the gitops-mr lane and seals as a gitops-mr
// ProposeSpec (repo + closed field edits), fail-closed without an operator propose mapping. The op-class is
// registered through the OVERLAY (a k8s-declarative class ships in no catalog yet — the mechanism is dark),
// and torn down after, so this test does not leak global state.

import (
	"encoding/json"
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/modules/actuation/gitopsmr"
)

const k8sDeclSlug = "k8s-set-replicas-test"

// registerK8sDeclarative installs a synthetic k8s-declarative op-class into the overlay for the test and
// tears it down after.
func registerK8sDeclarative(t *testing.T) {
	t.Helper()
	spec := opschema.OpClassSpec{
		OpClass:    k8sDeclSlug,
		Op:         "set replicas",
		Family:     opschema.FamilyServiceLifecycle,
		SafetyTier: opschema.TierLowReversible,
		EffectKind: opschema.EffectK8sDeclarative,
		Params:     []opschema.ParamSpec{{Name: "replicas", Required: true}},
	}
	hash, err := opschema.CanonicalHash(spec)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	accepted, rejected := opschema.SetOverlay([]opschema.OverlayEntry{{Spec: spec, Hash: hash}})
	if accepted != 1 {
		t.Fatalf("overlay register: accepted=%d rejected=%v (a k8s-declarative overlay class must validate)", accepted, rejected)
	}
	t.Cleanup(func() { opschema.SetOverlay(nil) })
}

func TestEffectKindRegimeRoutesK8sDeclarativeToGitOpsMR(t *testing.T) {
	registerK8sDeclarative(t)
	// KILLING MUTATION: drop the EffectK8sDeclarative case in effectKindRegime → byKind=false and this reddens.
	reg, byKind := effectKindRegime(k8sDeclSlug)
	if !byKind || reg != regime.RegimeGitOpsMR {
		t.Fatalf("a k8s-declarative op-class must route to %q by kind, got reg=%q byKind=%v", regime.RegimeGitOpsMR, reg, byKind)
	}
}

func TestSealEffectK8sDeclarativeEncodesProposeSpec(t *testing.T) {
	registerK8sDeclarative(t)
	d := Deps{GitOpsMRProposeForOpClass: func(op string, params map[string]string) (gitopsmr.ProposeSpec, bool) {
		if op != k8sDeclSlug {
			return gitopsmr.ProposeSpec{}, false
		}
		return gitopsmr.ProposeSpec{
			RepoID:  "infra-nl",
			OpClass: op,
			Edits:   []gitopsmr.FieldEdit{{FieldRuleID: "replicas-rule", NewValue: params["replicas"]}},
		}, true
	}}
	argv, stdin := sealEffect(d, manifest.Action{OpClass: k8sDeclSlug, Op: "set replicas", Params: map[string]string{"replicas": "3"}}, "cluster-nl")
	if len(argv) != 1 || argv[0] != gitopsmr.ProposeVerb {
		t.Fatalf("k8s-declarative argv = %v, want [%s]", argv, gitopsmr.ProposeVerb)
	}
	var spec gitopsmr.ProposeSpec
	if err := json.Unmarshal(stdin, &spec); err != nil {
		t.Fatalf("k8s-declarative stdin must decode to a ProposeSpec: %v", err)
	}
	if spec.RepoID != "infra-nl" || spec.OpClass != k8sDeclSlug {
		t.Fatalf("ProposeSpec = %+v, want repo infra-nl / this op-class", spec)
	}
	if len(spec.Edits) != 1 || spec.Edits[0].FieldRuleID != "replicas-rule" || spec.Edits[0].NewValue != "3" {
		t.Fatalf("ProposeSpec.Edits = %+v, want the one typed field edit replicas-rule=3", spec.Edits)
	}
}

func TestSealEffectK8sDeclarativeFailsClosedWithoutResolver(t *testing.T) {
	registerK8sDeclarative(t)
	act := manifest.Action{OpClass: k8sDeclSlug, Op: "set replicas", Params: map[string]string{"replicas": "3"}}
	// No resolver wired ⇒ empty effect ⇒ the gitops-mr leaf refuses.
	if argv, stdin := sealEffect(Deps{}, act, "cluster-nl"); argv != nil || stdin != nil {
		t.Fatalf("k8s-declarative with NO propose config must fail closed (nil,nil), got argv=%v stdin=%q", argv, stdin)
	}
	// A resolver that maps nothing for this op-class ⇒ fail closed.
	d := Deps{GitOpsMRProposeForOpClass: func(string, map[string]string) (gitopsmr.ProposeSpec, bool) {
		return gitopsmr.ProposeSpec{}, false
	}}
	if argv, stdin := sealEffect(d, act, "cluster-nl"); argv != nil || stdin != nil {
		t.Fatalf("k8s-declarative with ok=false must fail closed, got argv=%v stdin=%q", argv, stdin)
	}
	// A resolver returning an EMPTY repo id ⇒ EncodePropose fails ⇒ fail closed.
	d2 := Deps{GitOpsMRProposeForOpClass: func(op string, _ map[string]string) (gitopsmr.ProposeSpec, bool) {
		return gitopsmr.ProposeSpec{OpClass: op, Edits: []gitopsmr.FieldEdit{{FieldRuleID: "r", NewValue: "3"}}}, true
	}}
	if argv, _ := sealEffect(d2, act, "cluster-nl"); argv != nil {
		t.Fatalf("k8s-declarative with an empty repo id must fail closed, got argv=%v", argv)
	}
}
