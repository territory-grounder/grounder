package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// A DECISION WHOSE WORKFLOW IS GONE MUST STOP ADVERTISING ITSELF AS ACTIONABLE.
//
// pending_decision is written when a poll opens and cleared when the workflow records an outcome. A workflow
// that dies before resolving — a worker restart mid-deploy is the ordinary way — leaves its row open forever,
// and /v1/decisions lists it with caller_can_act = true.
//
// Measured live 2026-07-29: 13 of 136 open decisions were past the 24h VoteWait deadline, oldest 84.5h.
// Voting the three eldest returned HTTP 409 "no waiting decision for that ref" — the row claimed to be
// actionable and the workflow behind it did not exist.
//
// Gated on TG_TEST_POSTGRES_DSN (CI provides it).
func TestReapAbandonedClosesOnlyDecisionsPastTheDeadline(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the reaper test")
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
	st := &PendingStore{p: p}

	uniq := fmt.Sprintf("reap-%d", os.Getpid())
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	deadline := now.Add(-24 * time.Hour) // the VoteWait bound

	refs := map[string]time.Time{
		uniq + "-ancient": now.Add(-84 * time.Hour), // the 84.5h case
		uniq + "-stale":   now.Add(-30 * time.Hour),
		uniq + "-fresh":   now.Add(-6 * time.Hour), // inside the window: a human may still answer
		uniq + "-justnow": now.Add(-1 * time.Minute),
	}
	defer func() {
		for ref := range refs {
			if _, err := p.Exec(ctx, `DELETE FROM pending_decision WHERE external_ref = $1`, ref); err != nil {
				t.Errorf("cleanup %s: %v", ref, err)
			}
		}
	}()

	for ref, opened := range refs {
		if _, err := p.Exec(ctx, `
			INSERT INTO pending_decision (external_ref, action_id, band, approaches, prediction, reversible, site, opened_at, status)
			VALUES ($1,$2,'POLL_PAUSE','{}'::text[],'seeded',true,'dc1',$3,'open')`,
			ref, ref+"-act", opened); err != nil {
			t.Fatalf("seed %s: %v", ref, err)
		}
	}

	n, err := st.ReapAbandoned(ctx, deadline, now)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 2 {
		t.Errorf("reaped %d, want 2 (the 84h and 30h rows) — reaping fewer leaves phantoms in the console, "+
			"reaping more closes decisions a human could still answer", n)
	}

	open, err := st.OpenDecisions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	stillOpen := map[string]bool{}
	for _, d := range open {
		stillOpen[d.ExternalRef] = true
	}
	// THE CONVERSE, and it is the one that matters: reaping everything would also satisfy "no phantoms".
	for _, ref := range []string{uniq + "-fresh", uniq + "-justnow"} {
		if !stillOpen[ref] {
			t.Errorf("%s was reaped despite being INSIDE the vote window — a decision a human can still "+
				"answer must not be closed out from under them", ref)
		}
	}
	for _, ref := range []string{uniq + "-ancient", uniq + "-stale"} {
		if stillOpen[ref] {
			t.Errorf("%s is past the deadline and still listed as open — the console will keep offering an "+
				"approval whose workflow is gone, and the vote will 409", ref)
		}
	}

	// The outcome must be DISTINCT from human:timeout. A timeout is a fact about people not answering; this
	// is a fact about the poll ceasing to exist. Conflating them inflates the human-unresponsiveness signal
	// with an infrastructure failure and sends someone to fix the wrong thing.
	var outcome string
	if err := p.QueryRow(ctx, `SELECT outcome FROM pending_decision WHERE external_ref = $1`,
		uniq+"-ancient").Scan(&outcome); err != nil {
		t.Fatalf("read outcome: %v", err)
	}
	if outcome != "abandoned:no-workflow" {
		t.Errorf("outcome = %q, want abandoned:no-workflow", outcome)
	}
}

// Reaping must be idempotent and must never touch an already-resolved row: a second sweep is a no-op, and a
// decision a human genuinely answered keeps its human outcome forever.
func TestReapIsIdempotentAndNeverOverwritesAHumanOutcome(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database")
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
	st := &PendingStore{p: p}

	uniq := fmt.Sprintf("reap2-%d", os.Getpid())
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	answered, abandoned := uniq+"-answered", uniq+"-abandoned"
	defer func() {
		for _, ref := range []string{answered, abandoned} {
			if _, err := p.Exec(ctx, `DELETE FROM pending_decision WHERE external_ref = $1`, ref); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	}()

	for _, ref := range []string{answered, abandoned} {
		if _, err := p.Exec(ctx, `
			INSERT INTO pending_decision (external_ref, action_id, band, approaches, prediction, reversible, site, opened_at, status)
			VALUES ($1,$2,'POLL_PAUSE','{}'::text[],'seeded',true,'dc1',$3,'open')`,
			ref, ref+"-act", now.Add(-48*time.Hour)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// A human answered this one 47h ago — old enough to be caught by the deadline if status were ignored.
	if err := st.ResolveDecision(ctx, answered, answered+"-act", "human:approve", now.Add(-47*time.Hour)); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	first, err := st.ReapAbandoned(ctx, now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("reap 1: %v", err)
	}
	if first != 1 {
		t.Errorf("first sweep reaped %d, want 1 — the answered decision must be untouched", first)
	}
	second, err := st.ReapAbandoned(ctx, now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("reap 2: %v", err)
	}
	if second != 0 {
		t.Errorf("second sweep reaped %d, want 0 — reaping must be idempotent, or a periodic caller rewrites "+
			"resolved_at on every tick and the audit trail loses when the decision actually closed", second)
	}

	var outcome string
	if err := p.QueryRow(ctx, `SELECT outcome FROM pending_decision WHERE external_ref = $1`, answered).Scan(&outcome); err != nil {
		t.Fatalf("read: %v", err)
	}
	if outcome != "human:approve" {
		t.Errorf("a HUMAN-answered decision was overwritten to %q — a real vote must never be rewritten as an "+
			"infrastructure abandonment", outcome)
	}
}
