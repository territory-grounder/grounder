package db

// THE ATTRIBUTION RATE IS A NUMERATOR OVER AN EXISTING DENOMINATOR (TG-373 item 5).
//
// tg_ingest_source_recent_total already publishes how many alerts a source delivered in the baseline
// window — the denominator for its silence. What was missing is how many of them named NO MACHINE.
//
// Measured 2026-08-06, by hand against Postgres because there was no other way: 48 of 165
// prometheus-alertmanager rows (29.1%) had neither a host nor a subject address, and among them were the
// three alerts TG received about its own AWX outage. An unattributed incident cannot be blast-radius
// reasoned, deduped against the estate, or matched to its own ticket.
//
// Against a REAL Postgres: the whole mechanism is a windowed FILTER, and the window scoping is the part
// that can silently be wrong — an unattributed count over ALL time against a windowed total is a ratio of
// two different populations.

import (
	"context"
	"testing"
	"time"
)

func attributionFixture(ctx context.Context, t *testing.T) (*IngestFreshnessStore, *Pool, func()) {
	t.Helper()
	dsn := skipWithoutDB(t)
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	clean := func() { _, _ = p.Exec(ctx, `DELETE FROM ingest_alert WHERE source_id LIKE 'gold-attr-%'`) }
	clean()
	return NewIngestFreshnessStore(p), p, func() { clean(); p.Close() }
}

func seedAlert(ctx context.Context, t *testing.T, p *Pool, ref, source, host, ip string, age time.Duration) {
	t.Helper()
	var ipArg any
	if ip != "" {
		ipArg = ip
	}
	if _, err := p.Exec(ctx, `
		INSERT INTO ingest_alert (external_ref, source_type, source_id, alert_rule, severity, host, received_at, subject_ip)
		VALUES ($1, 'test', $2, 'r', 'warning', $3, now() - $4::interval, $5)`,
		ref, source, host, age.String(), ipArg); err != nil {
		t.Fatalf("seed %s: %v", ref, err)
	}
}

func freshnessFor(t *testing.T, rows []IngestFreshness, source string) IngestFreshness {
	t.Helper()
	for _, r := range rows {
		if r.SourceID == source {
			return r
		}
	}
	t.Fatalf("no freshness row for %q — the query did not return the source this test seeded", source)
	return IngestFreshness{}
}

// KILLING MUTATION: drop the recent_unattributed FILTER (the state this shipped in). RED — the rate is
// unpublished and reachable only by hand.
func TestUnattributedIsCountedBesideTheDenominator(t *testing.T) {
	ctx := context.Background()
	st, p, done := attributionFixture(ctx, t)
	defer done()

	seedAlert(ctx, t, p, "gold-attr-1", "gold-attr-src", "dc1pve01", "", time.Minute) // named host
	seedAlert(ctx, t, p, "gold-attr-2", "gold-attr-src", "", "10.0.2.193", time.Minute)   // named by address
	seedAlert(ctx, t, p, "gold-attr-3", "gold-attr-src", "", "", time.Minute)             // names NOTHING
	seedAlert(ctx, t, p, "gold-attr-4", "gold-attr-src", "", "", time.Minute)             // names NOTHING

	rows, err := st.Sources(ctx, time.Hour)
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	r := freshnessFor(t, rows, "gold-attr-src")
	if r.RecentTotal != 4 {
		t.Fatalf("RecentTotal = %d, want 4 — the denominator is wrong so the ratio means nothing", r.RecentTotal)
	}
	if r.RecentUnattributed != 2 {
		t.Errorf("RecentUnattributed = %d, want 2. A host counts as attributed, and so does a subject IP — "+
			"an incident named by address is not unattributed", r.RecentUnattributed)
	}
}

// THE WINDOW MUST SCOPE BOTH. An unattributed count over all time against a windowed total is a ratio of
// two different populations, and it inflates silently as history accumulates.
//
// KILLING MUTATION: remove the `received_at > now() - interval` clause from the unattributed FILTER only.
// RED — the old row outside the window is counted while the denominator excludes it.
func TestTheUnattributedCountIsScopedToTheSameWindowAsTheTotal(t *testing.T) {
	ctx := context.Background()
	st, p, done := attributionFixture(ctx, t)
	defer done()

	seedAlert(ctx, t, p, "gold-attr-recent", "gold-attr-win", "dc1pve01", "", time.Minute)
	seedAlert(ctx, t, p, "gold-attr-old", "gold-attr-win", "", "", 48*time.Hour) // unattributed, OUTSIDE

	rows, err := st.Sources(ctx, time.Hour)
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	r := freshnessFor(t, rows, "gold-attr-win")
	if r.RecentTotal != 1 {
		t.Fatalf("RecentTotal = %d, want 1 (only the in-window row)", r.RecentTotal)
	}
	if r.RecentUnattributed != 0 {
		t.Errorf("RecentUnattributed = %d, want 0 — a 48-hour-old row was counted against a 1-hour "+
			"denominator, so the published ratio would exceed 100%% and grow with history", r.RecentUnattributed)
	}
}

// A FULLY ATTRIBUTED SOURCE PUBLISHES ZERO, not nothing. Absent and zero must not mean the same thing.
func TestAFullyAttributedSourceStillReportsZero(t *testing.T) {
	ctx := context.Background()
	st, p, done := attributionFixture(ctx, t)
	defer done()

	seedAlert(ctx, t, p, "gold-attr-clean", "gold-attr-clean-src", "dc1pve01", "", time.Minute)

	rows, err := st.Sources(ctx, time.Hour)
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	r := freshnessFor(t, rows, "gold-attr-clean-src")
	if r.RecentTotal != 1 || r.RecentUnattributed != 0 {
		t.Fatalf("total=%d unattributed=%d, want 1/0", r.RecentTotal, r.RecentUnattributed)
	}
}
