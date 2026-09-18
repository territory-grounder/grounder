package db

// TG-483 — CollateralOpenedSince against a REAL Postgres (openFixture / TG_TEST_DSN): the semantics under
// drill are first-surfaced-since (a NOT EXISTS against per-delivery history) and the two exclusions, which
// are JOIN/window behavior a fake cannot vouch for. The pre-existing arm is this file's killing-mutation
// target: with the NOT EXISTS deleted, the already-firing sibling counts as collateral and that arm reds.

import (
	"context"
	"testing"
	"time"
)

func seedTG483Collateral(ctx context.Context, t *testing.T, p *Pool, base time.Time) func() {
	t.Helper()
	cleanup := func() {
		_, _ = p.Exec(ctx, `DELETE FROM ingest_alert_occurrence WHERE host LIKE 'tg483col-%'`)
	}
	cleanup() // a prior aborted run's rows must not fabricate hits
	ins := func(host, rule string, at time.Time) {
		t.Helper()
		if _, err := p.Exec(ctx, `
			INSERT INTO ingest_alert_occurrence (external_ref, alert_rule, severity, host, site, received_at)
			VALUES ($1, $2, 'warning', $3, 'nl', $4)`,
			"tg483col-"+host+"-"+rule, rule, host, at); err != nil {
			t.Fatalf("seed %s/%s: %v", host, rule, err)
		}
	}
	// The incident: web fires its own rule before AND after the heal (re-fire is flap business, not collateral).
	ins("tg483col-web", "NginxDown", base.Add(-30*time.Minute))
	ins("tg483col-web", "NginxDown", base.Add(2*time.Minute))
	// The genuine collateral: db first surfaces AFTER the heal, no prior history.
	ins("tg483col-db", "DiskFull", base.Add(3*time.Minute))
	// Pre-existing noise: cache was already firing before the heal and redelivers after.
	ins("tg483col-cache", "HighLoad", base.Add(-45*time.Minute))
	ins("tg483col-cache", "HighLoad", base.Add(4*time.Minute))
	// Outside the radius: a first-surfaced alert on a host NOT in the member list must not count.
	ins("tg483col-far", "DiskFull", base.Add(5*time.Minute))
	return cleanup
}

func TestCollateralOpenedSinceFirstSurfacedSemantics(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	base := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Microsecond)
	defer seedTG483Collateral(ctx, t, p, base)()
	s := NewAlertLogStore(p)
	members := []string{"tg483col-web", "tg483col-db", "tg483col-cache"}

	hits, err := s.CollateralOpenedSince(ctx, members, "tg483col-web", "NginxDown", base)
	if err != nil {
		t.Fatalf("CollateralOpenedSince: %v", err)
	}
	if len(hits) != 1 || hits[0].Host != "tg483col-db" || hits[0].AlertRule != "DiskFull" {
		t.Fatalf("exactly the first-surfaced sibling must count — own-rule re-fire excluded, pre-existing "+
			"redelivery excluded, out-of-radius ignored; got %+v", hits)
	}

	t.Run("empty member list asks nothing and returns nothing", func(t *testing.T) {
		hits, err := s.CollateralOpenedSince(ctx, nil, "tg483col-web", "NginxDown", base)
		if err != nil || hits != nil {
			t.Fatalf("no members ⇒ (nil, nil) — the CALLER decides what unobservable means, got %v err=%v", hits, err)
		}
	})
	t.Run("a quiet radius is an empty answer, not an error", func(t *testing.T) {
		hits, err := s.CollateralOpenedSince(ctx, []string{"tg483col-quiet1", "tg483col-quiet2"}, "tg483col-web", "NginxDown", base)
		if err != nil || len(hits) != 0 {
			t.Fatalf("a surveyed-quiet radius must answer empty, got %v err=%v", hits, err)
		}
	})
}
