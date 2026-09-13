package main

import (
	"os"
	"strings"
	"testing"
)

// Composition-root guard (TG-501): pin that wireOpclassClustering still wires the earned-catalog
// clustering pass — the candidate store, the dead-man liveness check, and the ready resolver — so the
// god-file carve that extracted it from main() cannot silently drop a piece. It returns nothing observable
// from outside the package (a fire-and-forget background loop gated on a database pool), so — the same
// reasoning worker_wiring_inventory_test.go and worker_model_budget_test.go rely on — the guard reads the
// source as text and asserts the wiring, rather than exercising a live database.

func TestWireOpclassClusteringWiresTheCandidacyPass(t *testing.T) {
	src, err := os.ReadFile("opclass_clustering_wiring.go")
	if err != nil {
		t.Fatalf("read opclass_clustering_wiring.go: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		`getenv("TG_OPCLASS_CLUSTER_INTERVAL"`,
		`db.NewOpClassCandidateStore(dbPool)`,
		`Liveness: func(ctx context.Context, window time.Duration) (opclasscluster.Liveness, error) {`,
		`Ready: opclasscluster.NewReadyResolver(estateHolder.Graph, nil)`,
		`go opclasscluster.RunPeriodically(context.Background(), clusterJob, d,`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wireOpclassClustering no longer wires %q — a clustering-pass piece was dropped in the carve", want)
		}
	}
}

// main() must actually CALL the extracted constructor — a carve that leaves it uncalled is a dark seam
// (present in the tree, absent from the process; the same class TG-315's authlog collector shipped as).
func TestMainCallsWireOpclassClustering(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(mainSrc), "wireOpclassClustering(dbPool, ledger, estateHolder)") {
		t.Error("main.go no longer calls wireOpclassClustering(dbPool, ledger, estateHolder) — the extracted opclass-clustering wiring is unreferenced")
	}
}
