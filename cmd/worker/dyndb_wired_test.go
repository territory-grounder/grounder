package main

import (
	"strings"
	"testing"
)

// TG-422: the dynamic-Postgres-credential wiring must stay present at the worker composition root. The `dyn:`
// scheme is registered only when TG_DYNDB_ADDR is set, and every lease is revoked at shutdown; a silent
// regression to "never registered" would leave the feature built-but-unreachable (the present-not-reaching
// defect), and dropping the Close would leak leases past process exit. This source guard pins all three, in
// the guest_liveness_wire_test.go house pattern (workerMainSource strips comment lines + a vacuity floor), so
// the pins anchor on CODE, not the explanatory comments.
//
// KILLING MUTATION: delete the `dyndb.Register(` call (or the `dynProvider.Close(` defer) — this test fails
// naming the gap. Restore → green.
func TestDynDBSchemeIsWiredFailClosed(t *testing.T) {
	src := workerMainSource(t)
	if !strings.Contains(src, "dyndb.Register(") {
		t.Error("the dyn: dynamic-credential scheme is not registered at the composition root — a dyn: DSN " +
			"would fail closed forever and the feature is built-but-unreachable (TG-422)")
	}
	if !strings.Contains(src, "TG_DYNDB_ADDR") {
		t.Error("the dyndb enable flag (TG_DYNDB_ADDR) is gone from main() — the scheme can no longer be armed (TG-422)")
	}
	if !strings.Contains(src, "dynProvider.Close(") {
		t.Error("the dyndb provider is not Closed at shutdown — leased credentials would not be revoked when " +
			"the worker exits, so a dynamic credential would outlive the process it was minted for (TG-422)")
	}
	// Slice 2: the plane DSN's dyn: branch must connect through the PER-CONNECTION credential seam, so the
	// pool survives lease rotation. KILLING MUTATION: route a dyn: plane DSN through db.Connect (one frozen
	// string) — both pins vanish and this names the regression.
	if !strings.Contains(src, "db.ConnectDynamic(") {
		t.Error("a dyn: plane DSN no longer connects through db.ConnectDynamic — the pool would freeze one " +
			"resolved lease and die at its max_ttl (TG-422 slice 2)")
	}
	if !strings.Contains(src, "dynProvider.Credentials(") {
		t.Error("the per-connection credential seam (Provider.Credentials) is not wired to the plane pool — " +
			"rotation would strand every connection opened after the first lease (TG-422 slice 2)")
	}
	// TG-553: the plane pool must be RECYCLED on lease rotation. A dropped lease role's live connections are not
	// killed but go UNPRIVILEGED (permission-denied on every table), so a connection dialed under the old lease
	// keeps failing until MaxConnLifetime (15m) unless the pool is Reset the instant the lease rotates. main()
	// must call the SHARED seam dyndb.ArmRotationEviction with pool.Reset — the same seam cmd/grounder uses (a
	// second dyn: pool that had the identical gap). KILLING MUTATION: delete the ArmRotationEviction call, or pass
	// a non-Reset func — a pin below names the regression (the ~3-hourly triage-plane permission-denied bursts return).
	if !strings.Contains(src, "dyndb.ArmRotationEviction(") {
		t.Error("main() no longer arms rotation-eviction — a connection dialed under a rotated-out lease would " +
			"fail permission-denied until MaxConnLifetime, not the instant the lease rotates (TG-553)")
	}
	if !strings.Contains(src, "pool.Reset") {
		t.Error("rotation-eviction is armed but not with pool.Reset — the pool's stale-lease connections are never " +
			"actually evicted when a lease rotates (TG-553)")
	}
}
