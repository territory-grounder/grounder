package main

import (
	"os"
	"strings"
	"testing"
)

// TG-422 slice 2: the grounder is the sole consumer of the migration DSN and its own runtime pool, so the
// dyn: wiring must stay present at THIS composition root too — the worker's wiring cannot resolve a DSN the
// worker never reads. Source guard in the house pattern (cmd/worker/dyndb_wired_test.go): pins anchor on
// code, and each names the regression it would mean.
//
// KILLING MUTATION: delete the wireDynDB() call, the dyn: migration resolution, or the ConnectDynamic
// runtime branch — the matching pin fails naming the gap. Restore → green.
func TestGrounderDynDBIsWiredFailClosed(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	var kept []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	src := strings.Join(kept, "\n")
	if len(src) < 1000 {
		t.Fatal("main.go read came back implausibly small — the guard would be vacuous")
	}
	if !strings.Contains(src, "wireDynDB()") {
		t.Error("wireDynDB() is not called at the grounder composition root — a dyn: migration/runtime DSN " +
			"could never resolve in the only process that uses them (TG-422 slice 2)")
	}
	if !strings.Contains(src, "dynProvider.Close(") {
		t.Error("the dyndb provider is not Closed at grounder shutdown — leased credentials would outlive " +
			"the process (TG-422)")
	}
	if !strings.Contains(src, "db.Migrate(ctx, migrationDSN)") {
		t.Error("db.Migrate no longer runs on the dyn-resolved migrationDSN — a dyn: migration DSN would " +
			"reach Migrate unresolved and fail every boot (TG-422 slice 2)")
	}
	if !strings.Contains(src, "db.ApplyPlaneGrants(ctx, migrationDSN)") {
		t.Error("ApplyPlaneGrants no longer shares the dyn-resolved migrationDSN (TG-422 slice 2)")
	}
	if !strings.Contains(src, "db.ConnectDynamic(") || !strings.Contains(src, "dynProvider.Credentials(") {
		t.Error("a dyn: runtime DSN no longer connects through the per-connection credential seam " +
			"(db.ConnectDynamic + Provider.Credentials) — the pool would freeze one lease and die at its " +
			"max_ttl (TG-422 slice 2)")
	}
	// TG-553: the grounder runtime pool is the SECOND dyn: pool (after cmd/worker) and had the identical gap —
	// a dropped lease role's live connections go UNPRIVILEGED, not dead, so a connection dialed under a
	// rotated-out lease fails permission-denied until MaxConnLifetime unless the pool is Reset on rotation.
	// KILLING MUTATION: delete the dyndb.ArmRotationEviction(dynProvider, role, pool.Reset, ...) call in main().
	if !strings.Contains(src, "dyndb.ArmRotationEviction(") || !strings.Contains(src, "pool.Reset") {
		t.Error("the grounder runtime pool is not recycled on lease rotation (dyndb.ArmRotationEviction with " +
			"pool.Reset) — a connection dialed under a rotated-out lease would fail permission-denied until " +
			"MaxConnLifetime, not the instant it rotates; cmd/worker had the same gap (TG-553)")
	}

	w, err := os.ReadFile("dyndb_wire.go")
	if err != nil {
		t.Fatalf("read dyndb_wire.go: %v", err)
	}
	if !strings.Contains(string(w), "TG_DYNDB_ADDR") || !strings.Contains(string(w), "dyndb.Register(") {
		t.Error("dyndb_wire.go lost the enable flag or the Register call — the scheme can no longer be " +
			"armed at the grounder (TG-422 slice 2)")
	}
}
