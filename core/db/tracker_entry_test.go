package db

// TG-490 store drills, against a REAL migrated Postgres (0097) in the throwaway drill database:
// every risk here is SQL semantics — the anti-join that defines "unfiled", the DISTINCT ON
// first-arrival pick, the window bound, the first-wins Ensure, and the monotone comment cursor.
//
// KILLING MUTATION (executed 2026-08-14): in Unfiled's SQL, drop `t.external_ref IS NULL` (keep
// the LEFT JOIN) — every recent incident then re-files on every pass, the duplicate-ticket storm
// the ledger exists to prevent. TestTrackerEntryUnfiledAntiJoin goes red on the filed row
// reappearing. Restored, green.

import (
	"context"
	"testing"
	"time"
)

func tg490SeedAlert(t *testing.T, p *Pool, ref, host string, age time.Duration) {
	t.Helper()
	if _, err := p.Exec(context.Background(), `
		INSERT INTO ingest_alert (external_ref, source_type, alert_rule, severity, host, site, summary, received_at)
		VALUES ($1, 'librenms', 'NginxDown', 'critical', $2, 'dc1', 'seeded', now() - make_interval(secs => $3))`,
		ref, host, age.Seconds()); err != nil {
		t.Fatalf("seed alert %s: %v", ref, err)
	}
}

// The two-phase lifecycle in real SQL: Reserve claims once (a second reserve reports not-mine);
// Complete binds first-wins (the loser learns the winner's id); a reserved row leaves the
// Unfiled list, joins no recovery comments, and surfaces via StaleReserved until completed.
func TestTrackerEntryReserveCompleteLifecycle(t *testing.T) {
	ctx := context.Background()
	p := chainDrillDB(t, ctx)
	s := NewTrackerEntryStore(p)

	tg490SeedAlert(t, p, "te-1", "web01", time.Minute) // the render inputs the stale-scan re-joins
	mine, err := s.Reserve(ctx, "te-1", "TGOPS", "librenms")
	if err != nil || !mine {
		t.Fatalf("first reserve must be mine: %v %v", mine, err)
	}
	if again, err := s.Reserve(ctx, "te-1", "TGOPS", "librenms"); err != nil || again {
		t.Fatalf("a second reserve must report not-mine (no error): %v %v", again, err)
	}
	stale, err := s.StaleReserved(ctx, 0, 10)
	if err != nil || len(stale) != 1 || stale[0].ExternalRef != "te-1" {
		t.Fatalf("an uncompleted reservation must surface as stale, got %+v err=%v", stale, err)
	}
	// Round-3 finding #3: the stale row carries the FULL render inputs re-joined from ingest —
	// a resolver-created ticket is a real ticket, never "[alert] (no host): (no rule)".
	if a := stale[0].Alert; a.Host != "web01" || a.AlertRule != "NginxDown" || a.Severity != "critical" || a.ExternalRef != "te-1" {
		t.Fatalf("the stale scan must re-join the incident's render inputs, got %+v", a)
	}
	won, id, err := s.Complete(ctx, "te-1", "TGOPS-1")
	if err != nil || !won || id != "TGOPS-1" {
		t.Fatalf("first completion must win: %v %q %v", won, id, err)
	}
	// The completion race: the loser gets the winner's id back (for the duplicate-closing path).
	won, id, err = s.Complete(ctx, "te-1", "TGOPS-999")
	if err != nil || won || id != "TGOPS-1" {
		t.Fatalf("second completion must lose and learn the winner: won=%v id=%q err=%v", won, id, err)
	}
	if stale, _ := s.StaleReserved(ctx, 0, 10); len(stale) != 0 {
		t.Fatalf("a completed row is not stale, got %+v", stale)
	}
	if _, err := s.Reserve(ctx, "", "TGOPS", ""); err == nil {
		t.Fatal("empty external_ref must refuse")
	}
	if _, found, _ := s.Get(ctx, "te-none"); found {
		t.Fatal("a missing entry reads found=false")
	}
}

func TestTrackerEntryUnfiledAntiJoin(t *testing.T) {
	ctx := context.Background()
	p := chainDrillDB(t, ctx)
	s := NewTrackerEntryStore(p)

	tg490SeedAlert(t, p, "te-new", "web01", time.Minute)      // recent, unfiled → must appear
	// (re-fires live in ingest_alert_occurrence — ingest_alert is UNIQUE per external_ref by
	// schema (ingest_alert_ref_uidx), which this drill discovered the loud way; the DISTINCT ON
	// in Unfiled is therefore belt-only.)
	tg490SeedAlert(t, p, "te-filed", "web02", time.Minute)    // recent but already filed → must NOT appear
	tg490SeedAlert(t, p, "te-ancient", "web03", 48*time.Hour) // outside the window → must NOT appear
	if _, err := s.Reserve(ctx, "te-filed", "TGOPS", "librenms"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Complete(ctx, "te-filed", "TGOPS-2"); err != nil {
		t.Fatal(err)
	}
	// A RESERVED (uncompleted) incident must ALSO leave the unfiled list — that is the whole fix:
	// the reservation, not the completion, is what blocks the blind second create.
	tg490SeedAlert(t, p, "te-reserved", "web04", time.Minute)
	if _, err := s.Reserve(ctx, "te-reserved", "TGOPS", "librenms"); err != nil {
		t.Fatal(err)
	}

	got, err := s.Unfiled(ctx, time.Hour, 20)
	if err != nil {
		t.Fatalf("unfiled: %v", err)
	}
	if len(got) != 1 || got[0].ExternalRef != "te-new" {
		refs := []string{}
		for _, u := range got {
			refs = append(refs, u.ExternalRef)
		}
		t.Fatalf("exactly the recent unfiled incident must surface once, got %v", refs)
	}
	if got[0].Host != "web01" || got[0].AlertRule != "NginxDown" {
		t.Fatalf("the row must carry the renderer's inputs, got %+v", got[0])
	}
}

func TestTrackerEntryRecoveryCursorIsMonotone(t *testing.T) {
	ctx := context.Background()
	p := chainDrillDB(t, ctx)
	s := NewTrackerEntryStore(p)

	if _, err := s.Reserve(ctx, "te-r", "TGOPS", "librenms"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Complete(ctx, "te-r", "TGOPS-3"); err != nil {
		t.Fatal(err)
	}
	// A RESERVED-only incident never receives comments (issue_id='' is excluded by the join).
	if _, err := s.Reserve(ctx, "te-res-only", "TGOPS", "librenms"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO ingest_transition (external_ref, kind) VALUES ('te-res-only','recovery')`); err != nil {
		t.Fatal(err)
	}
	var id1, id2 int64
	if err := p.QueryRow(ctx, `
		INSERT INTO ingest_transition (external_ref, kind, host, alert_rule) VALUES ('te-r','recovery','web01','NginxDown') RETURNING id`).Scan(&id1); err != nil {
		t.Fatal(err)
	}
	if err := p.QueryRow(ctx, `
		INSERT INTO ingest_transition (external_ref, kind, host, alert_rule) VALUES ('te-r','recovery','web01','NginxDown') RETURNING id`).Scan(&id2); err != nil {
		t.Fatal(err)
	}
	// A non-recovery kind and a recovery for an UNFILED incident must both be invisible.
	if _, err := p.Exec(ctx, `INSERT INTO ingest_transition (external_ref, kind) VALUES ('te-r','flap')`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO ingest_transition (external_ref, kind) VALUES ('te-unfiled','recovery')`); err != nil {
		t.Fatal(err)
	}

	recs, err := s.RecoveriesToComment(ctx, 10)
	if err != nil || len(recs) != 2 {
		t.Fatalf("both recoveries of the FILED incident must surface (and nothing else), got %d err=%v", len(recs), err)
	}
	if err := s.MarkCommented(ctx, "te-r", id1); err != nil {
		t.Fatal(err)
	}
	recs, err = s.RecoveriesToComment(ctx, 10)
	if err != nil || len(recs) != 1 || recs[0].TransitionID != id2 {
		t.Fatalf("the cursor must hide the commented transition, got %+v err=%v", recs, err)
	}
	// Monotone: a stale worker cannot move the cursor backwards.
	if err := s.MarkCommented(ctx, "te-r", id2); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkCommented(ctx, "te-r", id1); err != nil {
		t.Fatal(err)
	}
	recs, err = s.RecoveriesToComment(ctx, 10)
	if err != nil || len(recs) != 0 {
		t.Fatalf("a stale lower mark must not resurrect commented recoveries, got %+v err=%v", recs, err)
	}
}
