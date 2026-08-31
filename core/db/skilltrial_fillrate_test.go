package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
)

// ArmFillRate must count EXACTLY the population ArmScores counts (spec/014 REQ-1309/1313).
//
// The oracles below live against a real Postgres because every risk here is SQL semantics: whether the
// dimension and rubric predicates are actually IN the rate query. A fake store cannot fail this — the
// engine-level test in core/skillstore proves the engine ASKS per dimension; only this proves the
// database ANSWERS per dimension.
//
// The defect: until 2026-08-07 the rate was `COUNT(DISTINCT external_ref) FROM session_judgment WHERE
// judged_at > now() - window` — no dimension, no rubric, no score filter. On the running system that
// read 207.57/day against arms filling at 0.43/day for falsifiable_prediction, and trial 14 was
// created on the strength of the larger number.
//
// Gated on TG_TEST_POSTGRES_DSN (CI has no Postgres).
func TestArmFillRateCountsOnlyFillingSamples(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the arm fill-rate test")
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
	s := NewSkillStore(p)

	tag := fmt.Sprintf("fillrate-%d", os.Getpid())
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM session_judgment WHERE external_ref LIKE $1", tag+"%") }()

	cur := judge.RubricVersion()
	// Every row is inside the window and belongs to a distinct session, so the ONLY thing separating
	// them is the predicate under test.
	rows := []struct {
		ref, dim, rubric string
		score            float64
	}{
		{tag + "-a", "correct_diagnosis", cur, 4},   // counts
		{tag + "-b", "correct_diagnosis", cur, 3},   // counts
		{tag + "-c", "correct_diagnosis", cur, 0},   // unscored: cannot fill an arm
		{tag + "-d", "correct_diagnosis", "", 5},    // legacy rubric: pooled separately (TG-194)
		{tag + "-e", "correct_diagnosis", "1999", 5}, // some other rubric
		{tag + "-f", "falsifiable_prediction", cur, 5}, // a DIFFERENT dimension's supply
		{tag + "-g", "sensible_proposal", cur, 5},      // ditto
	}
	for _, r := range rows {
		if _, err := p.Exec(ctx, `
			INSERT INTO session_judgment (external_ref, dimension, score, rubric_version, judged_at)
			VALUES ($1, $2, $3, $4, now())
			ON CONFLICT (external_ref, dimension) DO UPDATE
			  SET score = EXCLUDED.score, rubric_version = EXCLUDED.rubric_version, judged_at = now()`,
			r.ref, r.dim, r.score, r.rubric); err != nil {
			t.Fatalf("seed %s/%s: %v", r.ref, r.dim, err)
		}
	}

	const window = 14 * 24 * time.Hour
	perDay := func(n float64) float64 { return n / 14.0 }

	got, err := s.ArmFillRate(ctx, "correct_diagnosis", window)
	if err != nil {
		t.Fatalf("arm fill rate: %v", err)
	}
	// a and b only: c is unscored, d and e carry other rubrics, f and g are other dimensions.
	if want := perDay(2); got != want {
		t.Errorf("correct_diagnosis: want %.4f/day (2 filling samples in 14d), got %.4f/day", want, got)
	}

	got, err = s.ArmFillRate(ctx, "falsifiable_prediction", window)
	if err != nil {
		t.Fatalf("arm fill rate: %v", err)
	}
	if want := perDay(1); got != want {
		t.Errorf("falsifiable_prediction: want %.4f/day (1 filling sample), got %.4f/day", want, got)
	}

	// A dimension nobody scores has NO supply. This is the vacuity case the guard depends on: an
	// unmeasured dimension must refuse a trial, never inherit the estate's overall traffic.
	got, err = s.ArmFillRate(ctx, "a_dimension_no_judge_scores", window)
	if err != nil {
		t.Fatalf("arm fill rate: %v", err)
	}
	if got != 0 {
		t.Errorf("an unscored dimension has no supply, got %.4f/day — the guard would project against traffic it cannot draw on", got)
	}
}
