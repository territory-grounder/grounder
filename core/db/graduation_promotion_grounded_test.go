package db

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TG-321, the half migration 0064 left open. 0064 bound `graduation_credit` to evidence; the AUTHORITY
// SURFACE — `policy_graduation.level`, which decides who may mutate production without a human vote —
// stayed unbound, written by a blanket upsert that any role with UPDATE can drive to `auto`.
//
// Measured in production 2026-08-07: restart-service, start-container and start-service all sit at `auto`
// with clean_run_count 0, against ZERO rows in graduation_credit. Silent autonomy with no grounding of
// any kind — history rather than an attack, but exactly the end state this ticket describes.
//
// Only a real Postgres executes a trigger; no in-memory fake can kill a mutation in this migration.
func gradPromoDB(t *testing.T) (*Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the graduation-promotion trigger oracle")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	for _, q := range []string{
		"DELETE FROM graduation_credit WHERE op_class LIKE 'tg321-%'",
		"DELETE FROM policy_graduation WHERE op_class LIKE 'tg321-%'",
		"DELETE FROM action_execution WHERE external_ref = 'tg321-seed-ref'",
	} {
		if _, err := p.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("clean fixture: %v", err)
		}
	}
	// 0064's trigger requires a credit to name an incident that PRODUCED an execution, so the fixture must
	// supply one. That the fixture needs this is itself the composition under test: promotion needs credit,
	// credit needs execution — a promotion is transitively grounded in something that actually ran.
	if _, err := p.Pool.Exec(ctx,
		`INSERT INTO action_execution (action_id, external_ref, verdict, unverifiable, target_host, site, executed_at, schema_version)
		 VALUES ('tg321-seed-action','tg321-seed-ref','match',false,'tg321-host','nl',now(),1)`); err != nil {
		t.Fatalf("seed execution: %v", err)
	}
	return p, ctx
}

func seedClass(t *testing.T, p *Pool, ctx context.Context, class, level string) {
	t.Helper()
	// Seed at `approve` (rank 1 from rank 0 is an advancement, so seeding needs a credit) — insert
	// directly with the trigger disabled is not available to a non-superuser, so seed via a grounded path.
	if _, err := p.Pool.Exec(ctx,
		`INSERT INTO graduation_credit (op_class, external_ref) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
		class, "tg321-seed-ref"); err != nil {
		t.Fatalf("seed credit: %v", err)
	}
	if _, err := p.Pool.Exec(ctx,
		`INSERT INTO policy_graduation (op_class, level, clean_run_count, notice_run_count, last_outcome, updated_at)
		 VALUES ($1,$2,0,0,'seeded',now())`, class, level); err != nil {
		t.Fatalf("seed class at %s: %v", level, err)
	}
	if _, err := p.Pool.Exec(ctx, `DELETE FROM graduation_credit WHERE op_class = $1`, class); err != nil {
		t.Fatalf("clear seed credit: %v", err)
	}
}

// TestUngroundedPromotionIsRefused is the finding.
func TestUngroundedPromotionIsRefused(t *testing.T) {
	p, ctx := gradPromoDB(t)
	const class = "tg321-ungrounded"
	seedClass(t, p, ctx, class, "approve")

	_, err := p.Pool.Exec(ctx, `UPDATE policy_graduation SET level='auto' WHERE op_class=$1`, class)
	if err == nil {
		t.Fatal("an op-class was promoted approve -> auto with ZERO graduation_credit rows. That is " +
			"autonomy granted by bookkeeping alone: the ladder decides who may mutate production without " +
			"a human vote, and its only writer is a blanket upsert any role with UPDATE can drive.")
	}
	if !strings.Contains(err.Error(), "TG-321") {
		t.Errorf("the refusal does not name the reason: %v", err)
	}

	// VACUITY FLOOR: the SAME promotion must SUCCEED once grounded, or this trigger is simply a wall and
	// the earn loop can never promote anything again.
	if _, err := p.Pool.Exec(ctx,
		`INSERT INTO graduation_credit (op_class, external_ref) VALUES ($1,$2)`, class, "tg321-seed-ref"); err != nil {
		t.Fatalf("insert credit: %v", err)
	}
	if _, err := p.Pool.Exec(ctx, `UPDATE policy_graduation SET level='auto' WHERE op_class=$1`, class); err != nil {
		t.Fatalf("a GROUNDED promotion was refused (%v) — the trigger blocks the legitimate earn loop, "+
			"which is an outage rather than a hardening", err)
	}
}

// TestDemotionAndSameLevelAreAlwaysAllowed. The safe direction must never be blocked, and Save() rewrites
// the whole row on every counter update — so a same-level write happens constantly and must not need credit.
func TestDemotionAndSameLevelAreAlwaysAllowed(t *testing.T) {
	p, ctx := gradPromoDB(t)
	const class = "tg321-demote"
	seedClass(t, p, ctx, class, "auto")

	if _, err := p.Pool.Exec(ctx,
		`UPDATE policy_graduation SET clean_run_count=5 WHERE op_class=$1`, class); err != nil {
		t.Fatalf("a SAME-LEVEL rewrite was refused (%v). Save() upserts the whole row on every counter "+
			"update, so this happens constantly and must never require credit.", err)
	}
	if _, err := p.Pool.Exec(ctx,
		`UPDATE policy_graduation SET level='approve' WHERE op_class=$1`, class); err != nil {
		t.Fatalf("a DEMOTION was refused (%v) — blocking the safe direction is strictly worse than the "+
			"defect this trigger closes", err)
	}
}

// TestSeedingDirectlyAtAutoIsRefused closes the same hole by its other door: if only UPDATE were
// constrained, a writer would simply DELETE the row and INSERT a fresh one at `auto`.
func TestSeedingDirectlyAtAutoIsRefused(t *testing.T) {
	p, ctx := gradPromoDB(t)
	_, err := p.Pool.Exec(ctx,
		`INSERT INTO policy_graduation (op_class, level, clean_run_count, notice_run_count, last_outcome, updated_at)
		 VALUES ('tg321-seed-auto','auto',0,0,'seeded',now())`)
	if err == nil {
		t.Fatal("a brand-new op-class was INSERTED straight at `auto` with no credit. Constraining only " +
			"UPDATE leaves DELETE-then-INSERT as an equivalent path to the same authority.")
	}
	// And the grounded insert must work.
	if _, err := p.Pool.Exec(ctx,
		`INSERT INTO graduation_credit (op_class, external_ref) VALUES ('tg321-seed-auto','tg321-seed-ref')`); err != nil {
		t.Fatalf("insert credit: %v", err)
	}
	if _, err := p.Pool.Exec(ctx,
		`INSERT INTO policy_graduation (op_class, level, clean_run_count, notice_run_count, last_outcome, updated_at)
		 VALUES ('tg321-seed-auto','auto',0,0,'seeded',now())`); err != nil {
		t.Fatalf("a GROUNDED insert at auto was refused: %v", err)
	}
	_, _ = p.Pool.Exec(ctx, `DELETE FROM policy_graduation WHERE op_class='tg321-seed-auto'`)
	_, _ = p.Pool.Exec(ctx, `DELETE FROM graduation_credit WHERE op_class='tg321-seed-auto'`)
}

// TestTheExistingProductionRowsAreNotBroken. The three live `auto` classes predate the credit mechanism.
// A trigger that refused their ordinary counter updates would take the ladder down on deploy.
func TestTheExistingProductionRowsAreNotBroken(t *testing.T) {
	p, ctx := gradPromoDB(t)
	const class = "tg321-legacy"
	seedClass(t, p, ctx, class, "auto") // at auto, with NO credits, exactly like production

	var credits int
	if err := p.Pool.QueryRow(ctx, `SELECT count(*) FROM graduation_credit WHERE op_class=$1`, class).Scan(&credits); err != nil {
		t.Fatalf("count credits: %v", err)
	}
	if credits != 0 {
		t.Fatalf("fixture is not the production shape: %d credit(s)", credits)
	}
	if _, err := p.Pool.Exec(ctx,
		`UPDATE policy_graduation SET clean_run_count=clean_run_count+1, last_outcome='verified_clean' WHERE op_class=$1`, class); err != nil {
		t.Fatalf("an ungrounded LEGACY class at auto could not record a clean run (%v). The three "+
			"production rows are in exactly this state; refusing their updates would break the ladder on "+
			"deploy rather than harden it.", err)
	}
}
