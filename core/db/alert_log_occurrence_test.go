package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/httpapi"
)

// TG-399: ingest_alert keeps ONE canonical row per external_ref (ON CONFLICT DO NOTHING, UPDATE-revoked), so a
// re-fire of a stable Alertmanager key (`am-<rule>-<host>`) reached AlertLogStore.Append on every delivery yet
// added nothing — re-fire volume was unrecordable by construction, which is why the recovery window looked
// empty. The append-only occurrence log (migration 0074) records each delivery, so occurrence count /
// first-seen / last-seen become derivable WITHOUT ever updating the canonical row.
//
// This oracle proves against real Postgres that three deliveries of the SAME external_ref leave the canonical
// ingest_alert row at exactly one (idempotency preserved) while the occurrence log carries all three, with
// honest first/last-seen. Killing mutation: drop the occurrence INSERT in Append, or give it ON CONFLICT
// DO NOTHING keyed on external_ref → Occurrences reports 1 (or 0) → RED.
func TestIngestAlertRefiresAreRecorded(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the durable alert-occurrence test")
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

	ref := fmt.Sprintf("am-Refire-dc1claude01-%d", os.Getpid())
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM ingest_alert WHERE external_ref = $1", ref)
		_, _ = p.Exec(ctx, "DELETE FROM ingest_alert_occurrence WHERE external_ref = $1", ref)
	}()

	store := NewAlertLogStore(p)
	// One first firing at t0, then two re-fires (the same stable external_ref) hours apart — exactly the
	// traffic shape the ticket describes (a fault re-firing for weeks against one canonical row).
	first := time.Unix(1_784_000_000, 0).UTC()
	second := first.Add(2 * time.Hour)
	third := first.Add(9 * time.Hour)
	for i, at := range []time.Time{first, second, third} {
		store.Append(ctx, httpapi.AlertRecord{
			ExternalRef: ref, SourceType: "alertmanager", AlertRule: "PVEPmxcfsWedged",
			Severity: "critical", Host: "dc1claude01", Site: "nl",
			Summary:    fmt.Sprintf("delivery %d", i+1),
			ObservedAt: at, ReceivedAt: at,
		})
	}

	// The canonical front-door record stays single — the append-only idempotency of ingest_alert is untouched.
	var canonical int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM ingest_alert WHERE external_ref = $1", ref).Scan(&canonical); err != nil {
		t.Fatalf("count canonical: %v", err)
	}
	if canonical != 1 {
		t.Fatalf("canonical ingest_alert row count = %d, want 1 (re-fires must NOT duplicate the canonical row)", canonical)
	}

	// The occurrence log carries every delivery — this is the recordability the ticket is about.
	occ, err := store.Occurrences(ctx, ref)
	if err != nil {
		t.Fatalf("Occurrences: %v", err)
	}
	if occ.Count != 3 {
		t.Fatalf("occurrence count = %d, want 3 — re-fire traffic is still unrecorded (the defect TG-399 fixes)", occ.Count)
	}
	if !occ.FirstSeen.Equal(first) {
		t.Errorf("first_seen = %v, want %v (the original firing time, not the latest)", occ.FirstSeen, first)
	}
	if !occ.LastSeen.Equal(third) {
		t.Errorf("last_seen = %v, want %v (the most recent re-fire, so a stale-vs-live incident is distinguishable)", occ.LastSeen, third)
	}

	// A never-seen ref reports an honest empty history, not an error.
	empty, err := store.Occurrences(ctx, ref+"-never")
	if err != nil {
		t.Fatalf("Occurrences(absent): %v", err)
	}
	if empty.Count != 0 || !empty.FirstSeen.IsZero() || !empty.LastSeen.IsZero() {
		t.Fatalf("absent ref must report zero history, got %+v", empty)
	}
}

// TG-427 oracle 1 — ATOMICITY: the canonical row must not survive a failed occurrence leg. Before the tx,
// a transient occurrence failure after a successful canonical INSERT was a PERMANENT divergence (the
// source's redelivery hits ON CONFLICT DO NOTHING, so the first occurrence is unrecoverable). The kill is
// executed for real: the occurrence table is renamed away so its INSERT fails, and the canonical row must
// vanish with it. Revert the tx (two independent Execs) and this goes RED with a stranded canonical row.
func TestAppendIsAtomicAcrossCanonicalAndOccurrence(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the durable alert-occurrence test")
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

	ref := fmt.Sprintf("tg427-atomic-%d", os.Getpid())
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM ingest_alert WHERE external_ref = $1", ref) }()

	// Make the occurrence leg fail: hide its table. Restored before any assertion so a panic cannot leave
	// the fixture broken for later tests (the rename-back also runs on the failure path via defer).
	if _, err := p.Exec(ctx, `ALTER TABLE ingest_alert_occurrence RENAME TO tg427_hidden`); err != nil {
		t.Fatalf("hide occurrence table: %v", err)
	}
	restored := false
	restore := func() {
		if !restored {
			_, _ = p.Exec(ctx, `ALTER TABLE tg427_hidden RENAME TO ingest_alert_occurrence`)
			restored = true
		}
	}
	defer restore()

	prev := occurrenceRetryDelay
	occurrenceRetryDelay = 50 * time.Millisecond
	defer func() { occurrenceRetryDelay = prev }()

	st := NewAlertLogStore(p)
	begin := time.Now()
	st.Append(ctx, httpapi.AlertRecord{ExternalRef: ref, SourceType: "test", SourceID: "tg427",
		AlertRule: "Atomic", Severity: "warning", Host: "h", Site: "s", ReceivedAt: time.Now().UTC()})
	// The close review's finding, pinned: Append runs in the live webhook goroutine, so the FAILURE path
	// must return as fast as the clean one — the retry belongs off this goroutine. Killing mutation: move
	// the sleep+re-attempt back inline and this trips before any row assertion.
	if el := time.Since(begin); el >= occurrenceRetryDelay {
		t.Fatalf("Append blocked %v on the failure path (>= the %v retry delay) — the retry is running "+
			"inline in the caller's goroutine again", el, occurrenceRetryDelay)
	}
	// Let the ASYNC retry fire against the still-hidden table — it must lose too — before restoring.
	time.Sleep(occurrenceRetryDelay + 300*time.Millisecond)
	restore()
	var canonical int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM ingest_alert WHERE external_ref = $1`, ref).Scan(&canonical); err != nil {
		t.Fatalf("count canonical: %v", err)
	}
	if canonical != 0 {
		t.Fatalf("canonical row survived a failed occurrence leg (count=%d) — the divergence TG-427 exists "+
			"to kill: this alert would exist forever with zero recorded occurrences", canonical)
	}
}

// TG-427 oracle 2 — RECOVERY: a transient failure heals on the bounded retry. The occurrence table is
// hidden for the FIRST attempt and restored inside the retry window; both rows must then exist. Kill:
// remove the retry from Append → the append is lost entirely (atomicity keeps it both-or-neither) → RED.
func TestAppendRecoversOnRetryAfterTransientFailure(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the durable alert-occurrence test")
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

	ref := fmt.Sprintf("tg427-recover-%d", os.Getpid())
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM ingest_alert WHERE external_ref = $1", ref)
		_, _ = p.Exec(ctx, "DELETE FROM ingest_alert_occurrence WHERE external_ref = $1", ref)
	}()

	if _, err := p.Exec(ctx, `ALTER TABLE ingest_alert_occurrence RENAME TO tg427_hidden`); err != nil {
		t.Fatalf("hide occurrence table: %v", err)
	}
	restored := false
	restore := func() {
		if !restored {
			_, _ = p.Exec(ctx, `ALTER TABLE tg427_hidden RENAME TO ingest_alert_occurrence`)
			restored = true
		}
	}
	defer restore()

	prev := occurrenceRetryDelay
	occurrenceRetryDelay = 400 * time.Millisecond // wide, deterministic window to restore the table in
	defer func() { occurrenceRetryDelay = prev }()

	st := NewAlertLogStore(p)
	// Append returns immediately now (the retry is a bounded async lane); restore the table inside the
	// 400ms window, then POLL for the healed rows — the retry lands on its own clock.
	st.Append(ctx, httpapi.AlertRecord{ExternalRef: ref, SourceType: "test", SourceID: "tg427",
		AlertRule: "Recover", Severity: "warning", Host: "h", Site: "s", ReceivedAt: time.Now().UTC()})
	restore()

	deadline := time.Now().Add(3 * time.Second)
	var canonical, occ int
	for time.Now().Before(deadline) {
		if err := p.QueryRow(ctx, `SELECT count(*) FROM ingest_alert WHERE external_ref = $1`, ref).Scan(&canonical); err != nil {
			t.Fatalf("count canonical: %v", err)
		}
		if err := p.QueryRow(ctx, `SELECT count(*) FROM ingest_alert_occurrence WHERE external_ref = $1`, ref).Scan(&occ); err != nil {
			t.Fatalf("count occurrence: %v", err)
		}
		if canonical == 1 && occ == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if canonical != 1 || occ != 1 {
		t.Fatalf("retry did not heal the transient failure within 3s: canonical=%d occurrence=%d, want 1/1", canonical, occ)
	}
}

// TG-427 oracle 3 — DistinctFirings discriminates provider re-fires from transport re-deliveries: same
// observed_at twice + a new observed_at once + one unknown-time delivery ⇒ deliveries 4, firings 2. Kill:
// count(DISTINCT received_at) or count(*) in its place → 4; dropping NULL-handling honesty → 3.
func TestOccurrencesCountsDistinctFirings(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the durable alert-occurrence test")
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

	ref := fmt.Sprintf("tg427-firings-%d", os.Getpid())
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM ingest_alert WHERE external_ref = $1", ref)
		_, _ = p.Exec(ctx, "DELETE FROM ingest_alert_occurrence WHERE external_ref = $1", ref)
	}()

	st := NewAlertLogStore(p)
	fire1 := time.Date(2026, 8, 10, 6, 0, 0, 0, time.UTC)
	fire2 := fire1.Add(5 * time.Minute)
	base := httpapi.AlertRecord{ExternalRef: ref, SourceType: "test", SourceID: "tg427",
		AlertRule: "Firings", Severity: "warning", Host: "h", Site: "s"}
	for _, obs := range []time.Time{fire1, fire1, fire2, {}} { // re-delivery repeats fire1; {} = unknown time
		rec := base
		rec.ObservedAt = obs
		rec.ReceivedAt = time.Now().UTC()
		st.Append(ctx, rec)
	}

	occ, err := st.Occurrences(ctx, ref)
	if err != nil {
		t.Fatalf("occurrences: %v", err)
	}
	if occ.Count != 4 || occ.DistinctFirings != 2 {
		t.Fatalf("deliveries/firings = %d/%d, want 4/2 — a transport re-delivery must not count as a new "+
			"firing, and an unknown-time delivery must count as a delivery only", occ.Count, occ.DistinctFirings)
	}
}
