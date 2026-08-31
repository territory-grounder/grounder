package db

import (
	"context"
	"testing"
	"time"
)

// ingest_alert IS AN INPUT TO THE AGENT'S WORLD MODEL, and nothing said so until it moved the ladder.
//
// ActiveHosts feeds the runner's common-cause corroboration — the evidence that separates an ISOLATED
// host-down from a shared-parent failure — which feeds the committed blast-radius prediction, which is
// scored into a verdict, which drives the graduation ladder. So the contents of ingest_alert reach all the
// way to whether an op-class is allowed to act unattended.
//
// That coupling is undocumented, and it bit on 2026-07-28. !671 made the pve-liveness detector write
// ingest_alert (so its detections could be credited by A1). Measured on dc1excalidraw01: EVERY
// start-guest blast-radius prediction before that change predicted an EMPTY cascade (0 hosts, at 16:40,
// 16:41, 18:30); the first prediction after it was non-empty (1 host), it deviated, and the spec/004
// auto-demote fired for the first time in the system's history 183ms later, dropping start-guest from AUTO
// to approve.
//
// The change was not wrong — a predictor that always predicts nothing never deviates and never predicts,
// which is exactly the "falsifiability at or below chance" reading the grounding surface publishes. What
// was wrong is that an INGEST change could reach the WORLD MODEL with nothing declaring the path.
//
// These oracles pin the coupling so the next change to what lands in ingest_alert discovers it here rather
// than in the ladder. They are deliberately about SHAPE, not values: what counts as corroborating evidence.

func seedActiveHosts(ctx context.Context, t *testing.T, p *Pool) func() {
	t.Helper()
	now := time.Now().UTC()
	cleanup := func() { _, _ = p.Exec(ctx, `DELETE FROM ingest_alert WHERE host LIKE 'coupling-%'`) }
	cleanup()
	add := func(host, srcType, srcID string, at time.Time) {
		if _, err := p.Exec(ctx, `
			INSERT INTO ingest_alert (external_ref, source_type, source_id, alert_rule, severity, host, received_at, observed_at)
			VALUES ($1,$2,$3,'Device-Down','critical',$4,$5,$5)`,
			"coupling-"+host+"-"+srcType, srcType, srcID, host, at); err != nil {
			t.Fatalf("seed %s/%s: %v", host, srcType, err)
		}
	}
	// The SAME fault, seen by the slow push path and by the fast poller.
	add("coupling-push", "librenms", "lnms", now.Add(-2*time.Minute))
	add("coupling-poll", "pve-liveness", "pve-liveness", now.Add(-2*time.Minute))
	add("coupling-old", "librenms", "lnms", now.Add(-48*time.Hour)) // outside any sane window
	return cleanup
}

// TestCorroborationCountsEVERYDetectorNotJustThePushPath is the oracle the demotion earned.
//
// ActiveHosts must treat a detection as corroborating evidence regardless of WHICH detector produced it. If
// it silently favoured one source_type, then adding or removing a detector would move blast-radius
// predictions — and therefore verdicts, and therefore the graduation ladder — with nothing in the ingest
// change hinting that it could.
func TestCorroborationCountsEveryDetectorNotJustThePushPath(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	defer seedActiveHosts(ctx, t, p)()

	got, err := NewAlertLogStore(p).ActiveHosts(ctx,
		[]string{"coupling-push", "coupling-poll", "coupling-absent"}, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ActiveHosts: %v", err)
	}
	if !got["coupling-push"] {
		t.Error("a librenms detection is not corroborating evidence — the push path has always counted")
	}
	if !got["coupling-poll"] {
		t.Error("a pve-liveness detection is NOT counted as corroborating evidence, but a librenms one is. " +
			"Corroboration would then depend on WHICH detector saw the fault, so adding a detector silently " +
			"changes blast-radius predictions, verdicts, and the graduation ladder")
	}
	if got["coupling-absent"] {
		t.Error("a host with no alert at all was reported as active — corroboration would see evidence that " +
			"does not exist")
	}
}

// TestCorroborationHonoursItsWindow — an ancient alert is not evidence that a host is failing NOW. Without
// the bound, every host that ever alerted would corroborate forever and predicted cascades would only grow.
func TestCorroborationHonoursItsWindow(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	defer seedActiveHosts(ctx, t, p)()

	got, err := NewAlertLogStore(p).ActiveHosts(ctx,
		[]string{"coupling-old"}, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ActiveHosts: %v", err)
	}
	if got["coupling-old"] {
		t.Error("a 48h-old alert corroborated a CURRENT incident — stale evidence must fall outside the " +
			"window, or predicted cascades grow monotonically and never shrink")
	}
}

// TestAnEmptyHostSetAsksNothing — the corroboration caller passes the candidate cascade; an empty candidate
// must not degenerate into "every host that alerted".
//
// NOTE ON ITS STRENGTH: the early-return in ActiveHosts is a FAST PATH, not the guarantee. `host = ANY(NULL)`
// already matches nothing, so deleting the guard leaves behaviour identical and a mutation control against it
// comes back GREEN — which it did. This test documents the intended contract and would catch a rewrite that
// changed the predicate itself (e.g. dropping the host filter); it does NOT prove the guard is load-bearing,
// and claiming otherwise would be exactly the vacuous control this file exists to avoid.
func TestAnEmptyHostSetAsksNothing(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	defer seedActiveHosts(ctx, t, p)()

	got, err := NewAlertLogStore(p).ActiveHosts(ctx, nil, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ActiveHosts: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("an empty candidate set returned %d active hosts — corroboration must answer about the "+
			"hosts it was asked about, never about the whole estate", len(got))
	}
}
