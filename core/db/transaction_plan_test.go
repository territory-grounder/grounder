package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// spec/030 T-030-2: the plan rows against real Postgres — atomic create with step bindings, idempotent
// re-compose on the content-addressed id, FORWARD-ONLY CAS on both machines (a replay cannot resurrect
// a terminal plan or re-pend an executed step), and the revert-failed question ("which steps remain
// applied") answerable from the rows. Unique ids; nothing deleted.
func TestTransactionPlanRowsAndForwardOnlyMachines(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to an empty database to run the transaction-plan round-trip")
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
	s := NewTransactionPlanStore(p)

	planID := fmt.Sprintf("plan-%d", time.Now().UnixNano())
	steps := []PlanStep{
		{Ordinal: 1, ActionID: planID + "-a1", OpClass: "start-service"},
		{Ordinal: 2, ActionID: planID + "-a2", OpClass: "restart-service"},
	}
	if err := s.Create(ctx, planID, "restart-then-verify-unit", "TG-plan-sess", steps); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Idempotent re-compose: same content-addressed id, no error, no duplicate steps.
	if err := s.Create(ctx, planID, "restart-then-verify-unit", "TG-plan-sess", steps); err != nil {
		t.Fatalf("re-create must be idempotent: %v", err)
	}
	r, ok, err := s.Get(ctx, planID)
	if err != nil || !ok {
		t.Fatalf("get: %v ok=%v", err, ok)
	}
	if r.State != "proposed" || len(r.Steps) != 2 || r.Steps[0].ActionID != planID+"-a1" || r.Steps[1].Ordinal != 2 {
		t.Fatalf("round-trip broke: %+v", r)
	}

	// The forward machine, step by step; every illegal or stale move refused.
	if ok, _ := s.Transition(ctx, planID, "proposed", "executing"); ok {
		t.Fatal("proposed→executing skips the approval — the machine must refuse the move outright")
	}
	if _, err := s.Transition(ctx, planID, "proposed", "committed"); err == nil {
		t.Fatal("an unlisted transition must error, not just miss")
	}
	for _, m := range [][2]string{{"proposed", "approved"}, {"approved", "executing"}, {"executing", "reverted"}} {
		ok, err := s.Transition(ctx, planID, m[0], m[1])
		if err != nil || !ok {
			t.Fatalf("legal move %v: ok=%v err=%v", m, ok, err)
		}
	}
	// Terminal is terminal: no listed move leaves "reverted", and a stale CAS from a replayed activity
	// misses rather than succeeding.
	if _, err := s.Transition(ctx, planID, "reverted", "executing"); err == nil {
		t.Fatal("nothing may leave a terminal state")
	}
	if ok, _ := s.Transition(ctx, planID, "approved", "executing"); ok {
		t.Fatal("a stale CAS (plan already past 'approved') must miss, never succeed")
	}

	// Step machine + the revert-failed question.
	if ok, err := s.TransitionStep(ctx, planID, 1, "pending", "executed"); err != nil || !ok {
		t.Fatalf("step 1 execute: ok=%v err=%v", ok, err)
	}
	if ok, err := s.TransitionStep(ctx, planID, 1, "executed", "compensate-failed"); err != nil || !ok {
		t.Fatalf("step 1 compensate-failed: ok=%v err=%v", ok, err)
	}
	if _, err := s.TransitionStep(ctx, planID, 2, "pending", "compensated"); err == nil {
		t.Fatal("a pending step cannot be compensated — it never executed")
	}
	r, _, _ = s.Get(ctx, planID)
	applied := 0
	for _, st := range r.Steps {
		if st.State == "executed" || st.State == "compensate-failed" {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("the rows must answer 'which steps remain applied' — want 1 (the compensate-failed one), got %d in %+v", applied, r.Steps)
	}

	if _, ok, err := s.Get(ctx, "plan-never-existed"); err != nil || ok {
		t.Fatalf("absence must read as absence: ok=%v err=%v", ok, err)
	}
}
