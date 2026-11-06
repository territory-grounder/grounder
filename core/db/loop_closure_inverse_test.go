package db

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// TG-348 / TG-404 — the bound-rollback loop is now watched. Before TG-404 gave an inverse a durable row
// (action_execution.inverts_action_id), CountLoopClosures deliberately EXCLUDED this loop because "an
// inverse ran" was not countable without parsing a log string. This oracle proves it is now first-class: a
// forward execution is a rollback candidate (Generated), an inverse is the closing step (Closed), and a
// system that has executed but never reverted reads as NeverClosed rather than as unmeasurable.
//
// Runs against a REAL Postgres (TG_TEST_DSN) with migration 0071 applied — the whole point is the SQL over
// the new column.
func TestBoundRollbackLoopIsCountedFromInverseExecutions(t *testing.T) {
	ctx := context.Background()
	p, err := Connect(ctx, skipWithoutDB(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	st := NewActionExecutionStore(p)
	clean := func() { _, _ = p.Exec(ctx, `DELETE FROM action_execution WHERE action_id LIKE 'tg348-%'`) }
	clean()
	defer clean()

	find := func() LoopClosure {
		t.Helper()
		loops, err := p.CountLoopClosures(ctx)
		if err != nil {
			t.Fatalf("count loop closures: %v", err)
		}
		for _, l := range loops {
			if l.Loop == "bound_rollback" {
				return l
			}
		}
		t.Fatal("CountLoopClosures returned no bound_rollback loop — the loop TG-404 unblocked is still a " +
			"blind spot in the register (TG-348)")
		return LoopClosure{}
	}

	// Two forward executions, no inverse: the ticket's exact "executions happened, 0 inverses run" state.
	for _, id := range []string{"tg348-fwd-1", "tg348-fwd-2"} {
		if err := st.Record(ctx, id, "inc", "host", "site", safety.VerdictMatch, true, ""); err != nil {
			t.Fatalf("record forward %s: %v", id, err)
		}
	}
	before := find()
	if before.Closed != 0 {
		t.Fatalf("bound_rollback Closed=%d with no inverse recorded, want 0 — a phantom closure would report "+
			"a loop as healthy that has never actually reverted anything", before.Closed)
	}
	if !before.NeverClosed() {
		t.Errorf("with %d forward executions and 0 inverses the loop is NOT flagged NeverClosed — that is the "+
			"exact 'untested path presented as a feature' state TG-348 exists to surface", before.Generated)
	}

	// Now one inverse runs: the loop closes once, and the register must SEE it (the whole reason TG-404
	// gave the inverse a durable row).
	if err := st.Record(ctx, "tg348-inv-1", "inc", "host", "site", safety.VerdictMatch, true, "tg348-fwd-1"); err != nil {
		t.Fatalf("record inverse: %v", err)
	}
	after := find()
	if after.Closed < 1 {
		t.Errorf("after an inverse ran, bound_rollback Closed=%d, want >=1 — the closing step executed and the "+
			"register did not count it, so 'has the rollback loop ever closed?' stays wrongly answered No", after.Closed)
	}
	if after.NeverClosed() {
		t.Error("the loop still reads NeverClosed after an inverse ran — a real closure must clear the flag, " +
			"or the signal cries wolf on a loop that is working")
	}
	// The inverse must NOT inflate the denominator (it is a closure, not a fresh rollback candidate).
	if after.Generated != before.Generated {
		t.Errorf("Generated moved %d -> %d when an INVERSE was recorded — a forward-only denominator must not "+
			"count inverses, or the loop can never read as fully closed", before.Generated, after.Generated)
	}
}
