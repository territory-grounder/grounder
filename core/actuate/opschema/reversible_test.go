package opschema

import "testing"

// TG-685: reversibility is the REGISTRY's declaration, made explicitly for every embedded class — never inferred
// from a model's claim. KILLING MUTATION: delete disk-grow's "reversible": false from opschema.json — RED here.
func TestEveryEmbeddedClassDeclaresReversibility(t *testing.T) {
	for _, s := range Specs() {
		if s.Reversible == nil {
			t.Errorf("%s declares no reversibility — every embedded class must say whether its effect can be undone", s.OpClass)
		}
	}
	for oc, want := range map[string]bool{
		"disk-grow": false, "k8s-set-replicas": false,
		"start-guest": true, "stop-guest": true, "start-service": true, "restart-service": true,
		"reload-service": true, "start-container": true, "restart-container": true,
	} {
		s, ok := Lookup(oc)
		if !ok || s.Reversible == nil || *s.Reversible != want {
			t.Errorf("%s must declare reversible=%v", oc, want)
		}
		if DeclaredIrreversible(oc) == want {
			t.Errorf("DeclaredIrreversible(%s) must be %v", oc, !want)
		}
	}
	if DeclaredIrreversible("no-such-class") {
		t.Error("an unregistered class is not reported here (it has no execution path and is refused elsewhere)")
	}
}

// An overlay row that declares nothing reads as IRREVERSIBLE: absence is never reversibility.
func TestAnOverlayClassWithoutADeclarationIsIrreversible(t *testing.T) {
	spec, _ := Lookup("start-service")
	spec.OpClass, spec.Reversible, spec.CommitConfirmed = "tg685-overlay-start", nil, nil
	hash, err := CanonicalHash(spec)
	if err != nil {
		t.Fatal(err)
	}
	if accepted, rejected := SetOverlay([]OverlayEntry{{Spec: spec, Hash: hash}}); accepted != 1 {
		t.Fatalf("overlay register: %v", rejected)
	}
	t.Cleanup(func() { SetOverlay(nil) })
	if !DeclaredIrreversible("tg685-overlay-start") {
		t.Fatal("an undeclared overlay class must read as irreversible")
	}
}
