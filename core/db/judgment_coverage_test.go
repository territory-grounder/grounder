package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
)

// TestJudgmentCoverageCountsScoredPerDimension is the TG-360 read guard.
//
// Two deterministic axes had graded 2 and 1 of 3,371 sessions; nothing published judged/eligible per dimension,
// so a silent axis was indistinguishable from a working one. JudgmentCoverage is the read that makes coverage
// visible. It counts session_judgment ROWS per dimension (a row means the axis scored that session; an N/A
// writes no row), joined to session_triage so the window bounds it.
//
// It uses a BEFORE/AFTER delta so residual goldtest data cannot skew the assertion — the delta is exactly this
// test's own seed. Runs against a REAL Postgres (TG_TEST_DSN): the whole mechanism is the GROUP BY + JOIN.
func TestJudgmentCoverageCountsScoredPerDimension(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()

	tri := NewTriageStore(p)
	axr := NewAxisReadStore(p)
	rv := judge.RubricVersion()

	pfx := fmt.Sprintf("tg360cov-%d-", os.Getpid())
	refs := []string{pfx + "a", pfx + "b", pfx + "c"}
	cleanup := func() {
		for _, r := range refs {
			_, _ = p.Exec(ctx, `DELETE FROM session_judgment WHERE external_ref = $1`, r)
			_, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, r)
		}
	}
	cleanup()
	defer cleanup()

	since := time.Now().Add(-time.Hour)
	before, beforeJudged, err := axr.JudgmentCoverage(ctx, since)
	if err != nil {
		t.Fatalf("coverage before: %v", err)
	}

	// Seed: 3 sessions, each judged on appropriate_band + evidence_grounded; ONE also on estate_grounded — the
	// deterministic axis this ticket is about, deliberately at a lower count than the LLM axes.
	for _, r := range refs {
		if err := tri.RecordTriage(ctx, judge.TriageRow{ExternalRef: r, Outcome: "proposed"}); err != nil {
			t.Fatalf("record triage %s: %v", r, err)
		}
		if err := tri.WriteJudgment(ctx, r, "appropriate_band", 4.5, "", rv); err != nil {
			t.Fatalf("write appropriate_band %s: %v", r, err)
		}
		if err := tri.WriteJudgment(ctx, r, "evidence_grounded", 4.6, "", rv); err != nil {
			t.Fatalf("write evidence_grounded %s: %v", r, err)
		}
	}
	if err := tri.WriteJudgment(ctx, refs[0], judge.DimEstateGrounded, 5.0, "", rv); err != nil {
		t.Fatalf("write estate_grounded: %v", err)
	}

	after, afterJudged, err := axr.JudgmentCoverage(ctx, since)
	if err != nil {
		t.Fatalf("coverage after: %v", err)
	}

	scoredOf := func(cov []DimCoverage, dim string) int {
		for _, c := range cov {
			if c.Dimension == dim {
				return c.Scored
			}
		}
		return 0
	}
	delta := func(dim string) int { return scoredOf(after, dim) - scoredOf(before, dim) }

	// VACUITY GUARD: the seed must have moved the numbers, or the deltas below prove nothing.
	if afterJudged-beforeJudged == 0 {
		t.Fatal("vacuity guard: the judged-session denominator did not move after seeding 3 judged sessions")
	}

	// Per-dimension scored deltas are exactly this test's seed, regardless of what else is in the window.
	if d := delta("appropriate_band"); d != 3 {
		t.Errorf("appropriate_band scored delta = %d, want 3 (3 sessions each scored once)", d)
	}
	if d := delta("evidence_grounded"); d != 3 {
		t.Errorf("evidence_grounded scored delta = %d, want 3", d)
	}
	// ★ The low-coverage deterministic axis is counted HONESTLY at its real (small) number — the whole point:
	// a query that miscounts here is what let a 1-of-3,371 axis look like a working one.
	if d := delta(judge.DimEstateGrounded); d != 1 {
		t.Errorf("estate_grounded scored delta = %d, want 1 (only one session carried it) — a miscount here is "+
			"exactly how a near-silent axis hid (TG-360)", d)
	}
	// The denominator counts DISTINCT sessions, not judgments: 3 sessions carrying 7 judgments between them
	// must move it by 3, never by 7.
	if d := afterJudged - beforeJudged; d != 3 {
		t.Errorf("judged-session denominator delta = %d, want 3 distinct sessions (not 7 judgments) — the "+
			"denominator must count sessions or every per-axis ratio is wrong", d)
	}
}
