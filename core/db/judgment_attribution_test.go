package db

// ORACLES FOR JUDGMENT ATTRIBUTION (TG-194 + TG-195, migration 0052). Against a REAL Postgres, never a
// fake — every risk here is SQL semantics: the scalar-subquery derivation, the exactly-one guard, and the
// ON CONFLICT SET list (the mapped risk: forgetting rubric_version there makes every re-judge write a new
// score under a stale stamp).

import (
	"context"
	"testing"
)

func judgmentFixture(ctx context.Context, t *testing.T) (*Pool, func()) {
	t.Helper()
	dsn := skipWithoutDB(t)
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	cleanup := func() {
		_, _ = p.Exec(ctx, `DELETE FROM session_judgment WHERE external_ref LIKE 'gold-jattr-%'`)
		_, _ = p.Exec(ctx, `DELETE FROM session_risk_audit WHERE external_ref LIKE 'gold-jattr-%'`)
		p.Close()
	}
	_, _ = p.Exec(ctx, `DELETE FROM session_judgment WHERE external_ref LIKE 'gold-jattr-%'`)
	_, _ = p.Exec(ctx, `DELETE FROM session_risk_audit WHERE external_ref LIKE 'gold-jattr-%'`)
	return p, cleanup
}

func seedAudit(ctx context.Context, t *testing.T, p *Pool, ref, actionID string) {
	t.Helper()
	if _, err := p.Exec(ctx, `
		INSERT INTO session_risk_audit (external_ref, risk_level, action_id, schema_version)
		VALUES ($1, 'low', $2, 1)`,
		ref, actionID); err != nil {
		t.Fatalf("seed audit: %v", err)
	}
}

func readJudgment(ctx context.Context, t *testing.T, p *Pool, ref string) (score float64, rubric, action string) {
	t.Helper()
	if err := p.QueryRow(ctx, `
		SELECT score, rubric_version, action_id FROM session_judgment
		WHERE external_ref = $1 AND dimension = 'correct_diagnosis'`, ref).
		Scan(&score, &rubric, &action); err != nil {
		t.Fatalf("read judgment: %v", err)
	}
	return
}

// KILLING MUTATIONS, one per leg: (a) drop the rubric stamp from the INSERT — RED on the stamp check;
// (b) drop the scalar subquery / the exactly-one HAVING — RED on the derive and refuse legs; (c) drop
// rubric_version or action_id from the ON CONFLICT SET list — RED on the re-judge leg, which is exactly
// the mapped risk (a re-judge writing a new score under a stale stamp).
func TestJudgmentCarriesRubricVersionAndDerivedAction(t *testing.T) {
	ctx := context.Background()
	p, cleanup := judgmentFixture(ctx, t)
	defer cleanup()
	st := &TriageStore{p: p}

	// LEG 1 — one sealed action: derived and stamped.
	seedAudit(ctx, t, p, "gold-jattr-one", "act-jattr-1")
	if err := st.WriteJudgment(ctx, "gold-jattr-one", "correct_diagnosis", 4, "c", "rv-test-1"); err != nil {
		t.Fatal(err)
	}
	if score, rv, act := readJudgment(ctx, t, p, "gold-jattr-one"); rv != "rv-test-1" || act != "act-jattr-1" || score != 4 {
		t.Fatalf("one-action session: score=%v rubric=%q action=%q, want 4/rv-test-1/act-jattr-1", score, rv, act)
	}

	// LEG 2 — zero sealed actions: '' is the true value, and the write still succeeds.
	if err := st.WriteJudgment(ctx, "gold-jattr-zero", "correct_diagnosis", 3, "c", "rv-test-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, act := readJudgment(ctx, t, p, "gold-jattr-zero"); act != "" {
		t.Fatalf("zero-action session bound to %q — a judgment of a session that acted on nothing has no action", act)
	}

	// LEG 3 — several sealed actions: refuse to guess.
	seedAudit(ctx, t, p, "gold-jattr-many", "act-jattr-a")
	seedAudit(ctx, t, p, "gold-jattr-many", "act-jattr-b")
	if err := st.WriteJudgment(ctx, "gold-jattr-many", "correct_diagnosis", 2, "c", "rv-test-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, act := readJudgment(ctx, t, p, "gold-jattr-many"); act != "" {
		t.Fatalf("multi-action session bound to %q — picking among several is an inference wearing a fact's costume", act)
	}

	// LEG 4 — a re-judge under a NEW rubric refreshes the stamp AND re-derives the action (the audit
	// row may have landed after the first judgment).
	seedAudit(ctx, t, p, "gold-jattr-zero", "act-jattr-late")
	if err := st.WriteJudgment(ctx, "gold-jattr-zero", "correct_diagnosis", 5, "c2", "rv-test-2"); err != nil {
		t.Fatal(err)
	}
	if score, rv, act := readJudgment(ctx, t, p, "gold-jattr-zero"); rv != "rv-test-2" || act != "act-jattr-late" || score != 5 {
		t.Fatalf("re-judge: score=%v rubric=%q action=%q, want 5/rv-test-2/act-jattr-late — the ON CONFLICT "+
			"SET list is not refreshing the attribution", score, rv, act)
	}

	// LEG 5 — an empty rubric version is refused outright: a new row that cannot say which rubric graded
	// it recreates the un-attributable pool this schema change exists to end.
	if err := st.WriteJudgment(ctx, "gold-jattr-one", "correct_diagnosis", 4, "c", ""); err == nil {
		t.Fatal("an empty rubric version was accepted")
	}
}
