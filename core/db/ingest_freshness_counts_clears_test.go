package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// TG-393. Per-source freshness read `ingest_alert` alone. RAISES live there; RECOVERIES live in
// `ingest_transition`, and there was no union — so a source whose current traffic is entirely recovery
// notifications reads as SILENT. That is not an edge case; it is the recovery phase of every incident,
// i.e. precisely when the estate is getting better.
//
// MEASURED ON PRODUCTION 2026-08-07: librenms-dc2's last raise was 04:26:06 and its last recovery
// 13:56:03 — NINE AND A HALF HOURS later — while AlertSourceWentSilent fired against it. A false
// positive on a healthy feed trains an operator to ignore the alert, and it buries the one genuine
// silence there is (pve-liveness, quiet since 2026-07-31, TG-350).
//
// Only a real Postgres executes the UNION and the NULL filter; no in-memory fake can kill a mutation
// in this query.
func TestFreshnessCountsRecoveriesNotJustRaises(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the ingest-freshness union oracle")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()
	for _, q := range []string{
		"DELETE FROM ingest_transition WHERE external_ref LIKE 'tg393-%'",
		"DELETE FROM ingest_alert WHERE external_ref LIKE 'tg393-%'",
	} {
		if _, err := p.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("clean: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = p.Pool.Exec(ctx, "DELETE FROM ingest_transition WHERE external_ref LIKE 'tg393-%'")
		_, _ = p.Pool.Exec(ctx, "DELETE FROM ingest_alert WHERE external_ref LIKE 'tg393-%'")
	})

	old := time.Now().UTC().Add(-9 * time.Hour)
	recent := time.Now().UTC().Add(-2 * time.Minute)

	// The production shape: a stale RAISE and a fresh RECOVERY from the same source.
	if _, err := p.Pool.Exec(ctx,
		`INSERT INTO ingest_alert (external_ref, source_id, source_type, alert_rule, severity, host, received_at, schema_version)
		 VALUES ('tg393-raise','tg393-src','librenms','r','critical','h',$1,1)`, old); err != nil {
		t.Fatalf("seed raise: %v", err)
	}
	if _, err := p.Pool.Exec(ctx,
		`INSERT INTO ingest_transition (external_ref, kind, source_id, host, alert_rule, received_at, schema_version)
		 VALUES ('tg393-clear','recovery','tg393-src','h','r',$1,1)`, recent); err != nil {
		t.Fatalf("seed recovery: %v", err)
	}

	find := func() IngestFreshness {
		t.Helper()
		rows, err := NewIngestFreshnessStore(p).Sources(ctx, 24*time.Hour)
		if err != nil {
			t.Fatalf("Sources: %v", err)
		}
		var hits int
		var got IngestFreshness
		for _, f := range rows {
			if f.SourceID == "tg393-src" {
				hits++
				got = f
			}
		}
		if hits == 0 {
			t.Fatal("the seeded source is absent from the freshness read entirely")
		}
		// THE UNION MUST COLLAPSE. Two arms means two rows per source unless the reader merges them; a
		// duplicated source double-counts in every consumer and lets the older arm win an ordering.
		if hits != 1 {
			t.Fatalf("the source appears %d times — UNION ALL was not collapsed per source", hits)
		}
		return got
	}

	got := find()
	if got.LastSeen.Before(recent.Add(-time.Minute)) {
		t.Errorf("last_seen = %s, want the RECOVERY at %s. Freshness read the raise only, so a source "+
			"delivering nothing but clears reads as silent — which is the recovery phase of every "+
			"incident, and it fires AlertSourceWentSilent on the healthiest feeds.",
			got.LastSeen.UTC(), recent)
	}
	if got.RecentTotal < 2 {
		t.Errorf("recent_total = %d, want both arms counted (1 raise + 1 recovery)", got.RecentTotal)
	}
	// A recovery must NOT inflate the unattributed numerator — that ratio is about alerts arriving with
	// no subject, and a clear names the incident it closes.
	if got.RecentUnattributed != 0 {
		t.Errorf("recent_unattributed = %d, want 0 — a recovery is not an unattributed alert",
			got.RecentUnattributed)
	}

	// A transition with NO source (every row written before migration 0068) must contribute nothing
	// rather than inventing a source or crashing the group-by.
	if _, err := p.Pool.Exec(ctx,
		`INSERT INTO ingest_transition (external_ref, kind, source_id, host, alert_rule, received_at, schema_version)
		 VALUES ('tg393-orphan','recovery',NULL,'h','r',now(),1)`); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}
	rows, err := NewIngestFreshnessStore(p).Sources(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("Sources after orphan: %v", err)
	}
	for _, f := range rows {
		if f.SourceID == "" {
			t.Error("a NULL-source transition produced an empty-string source row. Pre-0068 rows are " +
				"deliberately not backfilled; they must contribute NOTHING, not a phantom source.")
		}
	}
}
