package estate

import (
	"testing"
	"time"
)

// TG-655: BlastRadius carries chaos provenance along the path, like Learned, so a consumer that must act only
// on topology facts can tell a chaos-learned reach from a declared one.
func TestBlastRadiusMarksChaosPaths(t *testing.T) {
	g := NewGraph()
	until := time.Now().Add(time.Hour)
	root := Entity{Type: TypeHost, Name: "r1"}
	g.Upsert(Edge{From: Entity{Type: TypeHost, Name: "topo1"}, To: root, Rel: RelDependsOn, Confidence: 0.9, Source: SourceDeclared, ValidUntil: until})
	g.Upsert(Edge{From: Entity{Type: TypeHost, Name: "chaos1"}, To: root, Rel: RelDependsOn, Confidence: 0.9, Source: SourceChaos, ValidUntil: until})
	g.Upsert(Edge{From: Entity{Type: TypeHost, Name: "behind1"}, To: Entity{Type: TypeHost, Name: "chaos1"}, Rel: RelDependsOn, Confidence: 0.9, Source: SourceDeclared, ValidUntil: until})
	got := map[string]Impact{}
	for _, imp := range g.BlastRadius(root, 3) {
		got[imp.Entity.Name] = imp
	}
	if imp, ok := got["topo1"]; !ok || imp.Chaos {
		t.Fatalf("a declared dependent must be reached and NOT marked chaos, got %+v (ok=%v)", imp, ok)
	}
	if imp, ok := got["chaos1"]; !ok || !imp.Chaos {
		t.Fatalf("a chaos-edge dependent must be marked chaos, got %+v (ok=%v)", imp, ok)
	}
	if imp, ok := got["behind1"]; !ok || !imp.Chaos {
		t.Fatalf("a dependent reached THROUGH a chaos hop must be marked chaos (the path is a guess), got %+v (ok=%v)", imp, ok)
	}
}
