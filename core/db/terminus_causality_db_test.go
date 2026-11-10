package db

// TG-662 causality against a REAL Postgres (TG_TEST_DSN, migration 0122): the facts row round-trips the causality,
// the append-only terminus_record accepts a row and refuses a clean credit that is not tg-caused (the CHECK is the
// last line of defence), and the causality readers count what is there.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
)

func TestTerminusFactsCausalityRoundTrip(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	s := NewTriageStore(p)
	ref := fmt.Sprintf("%s-tg662c-%d", fixtureNS, time.Now().UnixNano())
	if err := s.RecordTriage(ctx, judge.TriageRow{ExternalRef: ref, Host: "web01", AlertRule: "NginxDown",
		Band: "AUTO", Outcome: "proposed", Proposed: true, Op: "start", OpClass: "start-service", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("record triage: %v", err)
	}
	if err := s.MarkTerminusFacts(ctx, ref, TerminusFacts{Mutated: true, ConfirmedClear: true, CollateralKnown: true,
		Causality: "injector-restore", CausalityEvidence: "injector: 1 restore(s)"}); err != nil {
		t.Fatalf("mark: %v", err)
	}
	f, found, err := s.TerminusFactsFor(ctx, ref)
	if err != nil || !found {
		t.Fatalf("facts: found=%v err=%v", found, err)
	}
	if f.Causality != "injector-restore" || f.CausalityEvidence != "injector: 1 restore(s)" {
		t.Fatalf("causality must round-trip, got %+v", f)
	}
	// A legacy write (no causality) reads "" — the consumers treat it as indeterminate.
	if err := s.MarkTerminusFacts(ctx, ref, TerminusFacts{Mutated: true, ConfirmedClear: true, CollateralKnown: true}); err != nil {
		t.Fatal(err)
	}
	if f, _, _ = s.TerminusFactsFor(ctx, ref); f.Causality != "" {
		t.Fatalf("an unclassified write must read empty (indeterminate downstream), got %q", f.Causality)
	}
	// The CHECK refuses an out-of-vocabulary value.
	if err := s.MarkTerminusFacts(ctx, ref, TerminusFacts{Mutated: true, Causality: "verified"}); err == nil {
		t.Fatal("an out-of-vocabulary causality must be refused by the column CHECK")
	}
}

func TestTerminusRecordAppendAndRead(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	s := NewTerminusRecordStore(p)
	ref := fmt.Sprintf("%s-tg662r-%d", fixtureNS, time.Now().UnixNano())
	now := time.Now().UTC().Truncate(time.Microsecond)
	rec := TerminusRecord{ExternalRef: ref, ActionID: "act-1", OpClass: "start-service", Host: "web01", TargetHost: "web01", Site: "nl",
		ExecutedAt: now.Add(-10 * time.Minute), ClearedAt: now, Executed: true, HasVerdict: true, Verdict: "match", ConfirmedClear: true,
		ObjectState: "healthy", Collateral: "quiet", CreditDisposition: "withheld:causality-injector-restore", Causality: "injector-restore",
		CausalityEvidence: "injector: 1 restore(s)"}
	if err := s.Append(ctx, rec); err != nil {
		t.Fatalf("append: %v", err)
	}
	rows, err := tg662RecordsFor(ctx, p, ref)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read: rows=%d err=%v", len(rows), err)
	}
	got := rows[0]
	if got.CreditDisposition != rec.CreditDisposition || got.Causality != rec.Causality || !got.ExecutedAt.Equal(rec.ExecutedAt) || !got.ClearedAt.Equal(rec.ClearedAt) || got.RecordedAt.IsZero() {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	// A second terminus for the same ref APPENDS — nothing is overwritten.
	rec.CreditDisposition, rec.Causality, rec.CreditClean, rec.CreditFed = "clean", "tg-caused", true, true
	if err := s.Append(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if rows, _ = tg662RecordsFor(ctx, p, ref); len(rows) != 2 || rows[0].CreditDisposition != "withheld:causality-injector-restore" || rows[1].CreditDisposition != "clean" {
		t.Fatalf("records must append in order, got %+v", rows)
	}
	// The CHECK is the last line of defence: a clean credit must be tg-caused.
	bad := rec
	bad.Causality = "indeterminate"
	if err := s.Append(ctx, bad); err == nil {
		t.Fatal("a clean credit with a non-tg-caused causality must be refused by the table CHECK")
	}
	// Zero times persist as NULL and read back zero.
	nul := rec
	nul.ExecutedAt, nul.ClearedAt, nul.CreditClean, nul.CreditFed, nul.CreditDisposition = time.Time{}, time.Time{}, false, true, "not-clean:no-verdict"
	if err := s.Append(ctx, nul); err != nil {
		t.Fatal(err)
	}
	if rows, _ = tg662RecordsFor(ctx, p, ref); !rows[2].ExecutedAt.IsZero() || !rows[2].ClearedAt.IsZero() {
		t.Fatalf("zero times must round-trip as zero, got %+v", rows[2])
	}
}

func TestCausalityReadersCountTheEvidence(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	s := NewCausalityStore(p)
	host := fmt.Sprintf("%s-tg662h-%d", fixtureNS, time.Now().UnixNano())
	// A fixed timeline in the past: the shared fixtures' recovery-feed cursor tests read "recent" rows globally, so
	// this test's seeded transitions must never be recent.
	now := time.Date(2020, 3, 1, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() {
		_, _ = p.Exec(context.Background(), `DELETE FROM ingest_transition WHERE host = $1`, host)
		_, _ = p.Exec(context.Background(), `DELETE FROM injected_fault WHERE host = $1`, host)
	})
	incident, executed, cleared := now.Add(-30*time.Minute), now.Add(-20*time.Minute), now.Add(-5*time.Minute)
	// One fault restored INSIDE the interval, one injected inside it, one restored long before (out of scope).
	for _, row := range []struct {
		injected time.Time
		restored *time.Time
	}{
		{incident.Add(-time.Minute), tg662PtrTime(executed.Add(5 * time.Minute))},
		{executed.Add(2 * time.Minute), nil},
		{now.Add(-3 * time.Hour), tg662PtrTime(now.Add(-2 * time.Hour))},
	} {
		if _, err := p.Exec(ctx, `INSERT INTO injected_fault (host, fault_type, injected_at, restored_at, restore_state) VALUES ($1, 'service-down', $2, $3, CASE WHEN $3::timestamptz IS NULL THEN 'none' ELSE 'restored' END)`,
			host, row.injected, row.restored); err != nil {
			t.Fatalf("seed injected_fault: %v", err)
		}
	}
	restores, injections, err := s.InjectorActivity(ctx, []string{host, "other-" + host}, incident, executed, cleared)
	if err != nil || restores != 1 || injections != 1 {
		t.Fatalf("injector activity: restores=%d injections=%d err=%v (want 1/1)", restores, injections, err)
	}
	if r, i, err := s.InjectorActivity(ctx, nil, incident, executed, cleared); err != nil || r != 0 || i != 0 {
		t.Fatalf("no hosts reads zero, got %d/%d err=%v", r, i, err)
	}
	// Recovery transitions: one BEFORE the execution for the rule family counts; an empty rule is an error.
	if _, err := p.Exec(ctx, `INSERT INTO ingest_transition (external_ref, host, alert_rule, kind, received_at) VALUES ($1, $2, 'NginxDown', 'recovery', $3)`,
		host+"-t1", host, executed.Add(-time.Minute)); err != nil {
		t.Fatalf("seed ingest_transition: %v", err)
	}
	if n, err := s.RecoveriesBetween(ctx, host, "NginxDown", incident, executed); err != nil || n != 1 {
		t.Fatalf("recoveries before execute: n=%d err=%v", n, err)
	}
	if n, err := s.RecoveriesBetween(ctx, host, "NginxDown", executed, cleared); err != nil || n != 0 {
		t.Fatalf("recoveries after execute must not count as before, got n=%d err=%v", n, err)
	}
	if _, err := s.RecoveriesBetween(ctx, host, "", incident, executed); err == nil {
		t.Fatal("an empty rule must be an error (the reader cannot scope), never a zero")
	}
}

func tg662PtrTime(t time.Time) *time.Time { return &t }

// tg662RecordsFor reads one session's terminus records oldest first (test-only — the production code only appends).
func tg662RecordsFor(ctx context.Context, p *Pool, externalRef string) ([]TerminusRecord, error) {
	rows, err := p.Query(ctx, `
		SELECT external_ref, action_id, op_class, host, target_host, site,
		       COALESCE(executed_at, 'epoch'::timestamptz), COALESCE(cleared_at, 'epoch'::timestamptz),
		       executed, has_verdict, verdict, confirmed_clear, clear_reason, object_state, collateral,
		       credit_disposition, credit_fed, credit_clean, causality, causality_evidence, window_state, recorded_at, source
		  FROM terminus_record WHERE external_ref = $1 ORDER BY id`, externalRef)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TerminusRecord
	for rows.Next() {
		var r TerminusRecord
		if err := rows.Scan(&r.ExternalRef, &r.ActionID, &r.OpClass, &r.Host, &r.TargetHost, &r.Site, &r.ExecutedAt, &r.ClearedAt,
			&r.Executed, &r.HasVerdict, &r.Verdict, &r.ConfirmedClear, &r.ClearReason, &r.ObjectState, &r.Collateral,
			&r.CreditDisposition, &r.CreditFed, &r.CreditClean, &r.Causality, &r.CausalityEvidence, &r.WindowState, &r.RecordedAt, &r.Source); err != nil {
			return nil, err
		}
		if r.ExecutedAt.Unix() == 0 {
			r.ExecutedAt = time.Time{}
		}
		if r.ClearedAt.Unix() == 0 {
			r.ClearedAt = time.Time{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TestTerminusRecordWindowRowIsAppendedOncePerWindow (TG-662 final review, finding 1): a commit-confirmed run's
// terminus row reads WITHHELD (window still armed) and the window's own resolution is what credits or demotes, so the
// window appends a row of its own (source='window'). A window resolves once per (external_ref, action_id) — the
// commit_confirm key — so a retried resolve tail appends nothing twice, while terminus rows keep appending (a second
// terminus for the same ref is a new terminus, never an overwrite).
//
// KILLING MUTATION (executed 2026-11-07, whole-package run, restored from a copy): drop the
// `ON CONFLICT (external_ref, action_id) WHERE source = 'window' DO NOTHING` clause from Append. RED: the second
// window append fails with a unique violation ("a retried window resolve must be a no-op").
func TestTerminusRecordWindowRowIsAppendedOncePerWindow(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	s := NewTerminusRecordStore(p)
	ref := fmt.Sprintf("%s-tg662w-%d", fixtureNS, time.Now().UnixNano())
	term := TerminusRecord{ExternalRef: ref, ActionID: "act-w", OpClass: "restart-service", Executed: true, HasVerdict: true,
		Verdict: "match", ConfirmedClear: true, Collateral: "quiet", CreditDisposition: "withheld:window-armed",
		Causality: "tg-caused", WindowState: "armed"}
	if err := s.Append(ctx, term); err != nil {
		t.Fatalf("terminus append: %v", err)
	}
	win := TerminusRecord{ExternalRef: ref, ActionID: "act-w", OpClass: "restart-service", Executed: true, HasVerdict: true,
		Verdict: "match", ConfirmedClear: true, Collateral: "quiet", CreditDisposition: "clean:window-confirmed",
		CreditFed: true, CreditClean: true, Causality: "tg-caused", WindowState: "confirmed", Source: TerminusRecordSourceWindow}
	if err := s.Append(ctx, win); err != nil {
		t.Fatalf("window append: %v", err)
	}
	if err := s.Append(ctx, win); err != nil {
		t.Fatalf("a retried window resolve must be a no-op, got %v", err)
	}
	if err := s.Append(ctx, term); err != nil {
		t.Fatalf("second terminus append: %v", err)
	}
	rows, err := tg662RecordsFor(ctx, p, ref)
	if err != nil {
		t.Fatal(err)
	}
	var terminus, window int
	for _, r := range rows {
		switch r.Source {
		case TerminusRecordSourceTerminus:
			terminus++
		case TerminusRecordSourceWindow:
			window++
			if r.CreditDisposition != "clean:window-confirmed" || !r.CreditFed || !r.CreditClean || r.WindowState != "confirmed" {
				t.Fatalf("window row mismatch: %+v", r)
			}
		default:
			t.Fatalf("unexpected source %q", r.Source)
		}
	}
	if terminus != 2 || window != 1 {
		t.Fatalf("want 2 terminus rows (append-only) and exactly 1 window row, got terminus=%d window=%d", terminus, window)
	}
	bad := win
	bad.ActionID, bad.Causality = "act-w2", "external"
	if err := s.Append(ctx, bad); err == nil {
		t.Fatal("a clean window row with a non-tg-caused causality must be refused by the table CHECK")
	}
}
