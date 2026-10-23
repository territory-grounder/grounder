package db

// TG-444: edge_disproof.decayed_to is a DECAY-TIME SNAPSHOT, not the edge's live confidence. Since TG-388
// the source recompute (LaplaceConfidence over the learner-decayed counts, every 5-min refresh) supersedes
// the graph-side oldConfidence*factor this column records, so any reader trusting it as current confidence
// is wrong. The fix is truth-in-labeling; this pins the label so it cannot silently revert to the 0075
// wording ("the edge confidence AFTER this pass's decay") that reads as live.
//
// KILLING MUTATION (executed 2026-08-11): point migration 0081 at the down-migration wording (restore the
// 0075 label) — this test fails, because the live column comment then reads as live confidence again.

import (
	"context"
	"strings"
	"testing"
)

func TestDecayedToColumnIsLabelledTransient(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	var comment string
	// col_description over the table's oid + the column's ordinal position — the live schema comment 0081 set.
	if err := pool.Pool.QueryRow(ctx, `
		SELECT COALESCE(col_description('edge_disproof'::regclass, a.attnum), '')
		FROM pg_attribute a
		WHERE a.attrelid = 'edge_disproof'::regclass AND a.attname = 'decayed_to'`).Scan(&comment); err != nil {
		t.Fatalf("read column comment: %v", err)
	}
	if comment == "" {
		t.Fatal("edge_disproof.decayed_to carries NO column comment — the TG-444 relabel (0081) did not apply")
	}
	// The label must say it is a snapshot/superseded, and must NOT read as the live confidence.
	lc := strings.ToLower(comment)
	if !strings.Contains(lc, "snapshot") || !strings.Contains(lc, "supersede") || !strings.Contains(lc, "tg-444") {
		t.Fatalf("decayed_to comment does not carry the transient-snapshot relabel (want snapshot/supersede/TG-444): %q", comment)
	}
	// The exact pre-TG-444 wording that reads as live confidence must be gone.
	if strings.Contains(comment, "the edge confidence AFTER this pass's decay") {
		t.Fatalf("decayed_to comment reverted to the 0075 wording that reads as LIVE confidence: %q", comment)
	}
}
