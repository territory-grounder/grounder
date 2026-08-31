package eval

import (
	"sort"
	"testing"

	"github.com/territory-grounder/grounder/core/predict"
)

// TestEstateGraphDirection proves the eval now drives predictions from the REAL estate graph (all 383 edges,
// correct direction) rather than the former inverted 11-edge flat adjacency: a dependency parent's blast
// radius names its dependents, a switch names the access points that depend on it, and an unknown host fails
// closed to an empty prediction. Pure — runs in `make all` with no gateway.
func TestEstateGraphDirection(t *testing.T) {
	g := loadEstateGraph(t, "estate_fixture.json")
	m := &predict.InfragraphModel{Estate: g, DefaultRules: []string{"HostDown"}, MaxDepth: 3}

	// redis03 is a depends_on parent (nextcloud01 depends_on redis03); its blast radius MUST name nextcloud01.
	// The old adj[From]=To inversion returned EMPTY here — the exact bug this fixes.
	pred := m.Predict("a", "seed-1", "dc1redis03", "dc1", true)
	if _, ok := pred.PredictedHosts["dc1nextcloud01"]; !ok {
		t.Fatalf("redis03 blast radius must name its dependent nextcloud01; got %v", sortedKeys(pred.PredictedHosts))
	}

	// sw01 (switch) has six dependents (aps + syno); its blast radius must be non-trivial, not empty/backwards.
	sw := m.Predict("b", "seed-2", "dc1sw01", "dc1", true)
	if len(sw.PredictedHosts) < 3 {
		t.Fatalf("sw01 blast radius must name the hosts that depend on it; got %v", sortedKeys(sw.PredictedHosts))
	}

	// an unknown host resolves to nothing → empty prediction (fail-closed at prediction.go).
	empty := m.Predict("c", "seed-3", "dc1ghost99-nonexistent", "dc1", true)
	if len(empty.PredictedHosts) != 0 {
		t.Fatalf("unknown host must fail closed to an empty prediction; got %v", sortedKeys(empty.PredictedHosts))
	}
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
