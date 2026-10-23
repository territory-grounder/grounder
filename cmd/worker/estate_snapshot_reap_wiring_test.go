package main

// THE REAPER MUST BE CALLED, NOT MERELY WRITTEN (TG-355).
//
// core/db has seven oracles for reap_estate_snapshot against a real Postgres — the per-plane floor, the
// 24-hour window, the daily sample, the clamp, the journal. Every one of them passes on a binary that never
// calls it. This session has found the resolver-guarded/wiring-unguarded shape eleven times; a retention
// path that is never scheduled is the same defect wearing a different hat, and its symptom is a table that
// keeps growing while a test suite reports the reaper works.
//
// So this reads the composition root, comment-stripped — the block above it explains the design and names
// the function in prose, and a comment is not a call.

import (
	"os"
	"strings"
	"testing"
)

func workerMainCode(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	var kept []string
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "//") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}

// KILLING MUTATION: delete the snapshot-reaper goroutine from main(). RED — core/db's seven oracles all
// still pass, and estate_snapshot grows at 3.9 MB/day forever.
func TestTheSnapshotReaperIsScheduled(t *testing.T) {
	code := workerMainCode(t)

	for _, want := range []string{
		"db.NewEstateSnapshotReapStore(pool)", // constructed at the composition root
		"snapshotReaper.Reap(sctx",            // and actually CALLED on the ticker
		"time.NewTicker(snapshotReapEvery)",   // on a schedule, not once at boot
	} {
		if !strings.Contains(code, want) {
			t.Errorf("cmd/worker/main.go does not contain %q — the retention path exists in core/db and "+
				"nothing runs it, so the table this ticket is about keeps growing while its tests pass", want)
		}
	}

	// VACUITY FLOOR: comment-stripping that returned nothing would make every assertion above fail for the
	// wrong reason, and a stripper that returned the file unchanged would let a mention in prose satisfy it.
	if len(code) < 50000 {
		t.Fatalf("main.go read back as %d bytes after comment-stripping — the reader is broken", len(code))
	}
	if strings.Contains(code, "// ESTATE-SNAPSHOT RETENTION (TG-355)") {
		t.Fatal("comment lines survived the strip, so a design comment naming Reap would satisfy the " +
			"assertions above without a single call existing")
	}
}

// The operator-facing boot line must state the DATABASE floor, not only the configured value. An operator
// who sets TG_ESTATE_SNAPSHOT_KEEP_PER_PLANE=1 and reads "retention 1 row per plane" would believe a
// retention this deployment does not have.
//
// KILLING MUTATION: drop db.MinKeepPerPlane from the log line. RED.
func TestTheBootLineNamesTheDatabaseFloor(t *testing.T) {
	code := workerMainCode(t)
	if !strings.Contains(code, "db.MinKeepPerPlane") {
		t.Error("the estate-snapshot retention boot line does not name db.MinKeepPerPlane. The database " +
			"clamps anything below it, so a configured value printed alone tells an operator a retention " +
			"the deployment will not honour")
	}
	if !strings.Contains(code, "TG_ESTATE_SNAPSHOT_KEEP_PER_PLANE") {
		t.Error("the boot line does not name the env key an operator would edit")
	}
}
