package db

import (
	"context"
	"os"
	"testing"
)

// TG-458: TG-456's site-vocabulary canonicalization (core/ingest/site.go CanonicalizeSite) was
// WRITE-FORWARD ONLY — it folded new writes and left ~2,400 pre-existing rows in their legacy spellings
// ('NL', 'nl', 'gr', ...). Migration 0083 backfills them to the deployment-key form (dc1 / dc2) with
// the SAME fold, in pure SQL. A stored 'NL' that never became 'dc1' is not cosmetic: core/predict's
// ScoreControl compares alert.Site to prediction.Site as plain strings, so 'NL' vs 'dc1' reads as
// CROSS-site and a genuine same-site host is dropped from the negative control.
//
// This seeds ingest_alert — the widest spelling variety at the front door — with a row per case, applies
// the ACTUAL 0083 up.sql against a real Postgres, and proves the fold hits exactly the legacy nl/gr
// spellings while leaving every unknown / empty / already-canonical row untouched, and that a second
// application changes ZERO rows.
//
// WHY DB-GATED: the fix is pure SQL over a real text column; an in-memory twin would test a copy of the
// logic, not the migration. Gated on TG_TEST_POSTGRES_DSN (the empty-fixture family that calls Migrate
// itself — see core/db/dsn_gate_test.go), and it runs the migration under the DDL/owner role, which is why
// the UPDATE is not blocked by ingest_alert's `REVOKE UPDATE, DELETE ... FROM tg_runtime`.
//
// KILLING MUTATION (executed 2026-08-13): replace the body of 0083_canonicalize_legacy_site.up.sql with a
// no-op ("SELECT 1;"). The seeded 'NL'/'nl'/'gr' rows are then left legacy and this test fails on the first
// fold assertion — `ingest_alert row seeded "NL" = "NL" after 0083, want "dc1"`.
func TestSiteCanonicalizeBackfill0083(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to an empty Postgres to run the 0083 site-backfill migration test")
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

	// A unique external_ref namespace so this test seeds, asserts and cleans up ONLY its own rows on the
	// shared translog fixture — and never depends on or perturbs another test's data.
	const pfx = "tg458-site-"
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM ingest_alert WHERE external_ref LIKE $1", pfx+"%") }()
	if _, err := p.Exec(ctx, "DELETE FROM ingest_alert WHERE external_ref LIKE $1", pfx+"%"); err != nil {
		t.Fatalf("pre-clean residue from an earlier run: %v", err)
	}

	// One row per case: key = the external_ref suffix; seed = the site as STORED before 0083; want = after.
	cases := []struct{ key, seed, want string }{
		{"nl-upper", "NL", "dc1"},     // LibreNMS / pve-liveness spelling -> folds
		{"nl-lower", "nl", "dc1"},     // Prometheus / k8s label spelling  -> folds
		{"nl-canon", "dc1", "dc1"}, // already canonical -> UNCHANGED (the `site <> target` guard's job)
		{"gr-lower", "gr", "dc2"},     // folds
		{"gr-canon", "dc2", "dc2"}, // already canonical -> UNCHANGED
		{"unknown-ch", "ch", "ch"},       // not a declared site -> honest passthrough
		{"unknown-no", "no", "no"},       // not a declared site -> passthrough (and must NOT be mistaken for 'nl')
		{"empty", "", ""},                // empty stays empty
	}
	for _, c := range cases {
		if _, err := p.Exec(ctx,
			`INSERT INTO ingest_alert (external_ref, site) VALUES ($1, $2)`, pfx+c.key, c.seed); err != nil {
			t.Fatalf("seed %s (site=%q): %v", c.key, c.seed, err)
		}
	}

	// APPLY THE REAL MIGRATION. readMigration reads 0083 off disk; Exec runs its UPDATEs against the seeded
	// rows above. It is idempotent, so re-running it here after Migrate already applied it once is harmless.
	// If the migration body is neutralized, the legacy rows stay legacy and the fold assertions go RED.
	up := readMigration(t, "0083_canonicalize_legacy_site.up.sql")
	if _, err := p.Exec(ctx, up); err != nil {
		t.Fatalf("apply 0083 up.sql: %v", err)
	}

	got := func(key string) string {
		var site string
		if err := p.Pool.QueryRow(ctx,
			`SELECT site FROM ingest_alert WHERE external_ref = $1`, pfx+key).Scan(&site); err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		return site
	}
	for _, c := range cases {
		if s := got(c.key); s != c.want {
			t.Errorf("ingest_alert row seeded %q = %q after 0083, want %q", c.seed, s, c.want)
		}
	}

	// IDEMPOTENCY, MEASURED. Re-run 0083's exact dc1 fold for this table, scoped to the seeded rows, and
	// assert it changes ZERO rows — the `site <> 'dc1'` guard means a second pass has nothing to do. The
	// site literals and the normalized-key expression are exactly migration 0083's; the only added clause is
	// the external_ref scope (a bound parameter, not string-built SQL).
	tag, err := p.Exec(ctx, `
		UPDATE ingest_alert SET site = 'dc1'
		  WHERE external_ref LIKE $1
		    AND site <> 'dc1'
		    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'nl'
		      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'nllei%')`,
		pfx+"%")
	if err != nil {
		t.Fatalf("idempotency re-run: %v", err)
	}
	if n := tag.RowsAffected(); n != 0 {
		t.Errorf("a second application of the dc1 fold changed %d rows, want 0 — 0083 is not idempotent", n)
	}

	// And a full second application of the whole migration must leave every seeded value exactly as the
	// first pass left it (behavioural idempotency across all nine tables' statements at once).
	if _, err := p.Exec(ctx, up); err != nil {
		t.Fatalf("re-apply 0083 up.sql: %v", err)
	}
	for _, c := range cases {
		if s := got(c.key); s != c.want {
			t.Errorf("after a SECOND 0083 apply, row seeded %q = %q, want %q (idempotency broken)", c.seed, s, c.want)
		}
	}
}
