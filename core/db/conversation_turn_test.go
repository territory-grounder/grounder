package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TG-80 P2-8: the conversation store against real Postgres — append/read-back newest-first, the asking
// session excluded, expired turns invisible to Recent and deletable ONLY through the 0109 SECURITY
// DEFINER reap (bounded, expired-only). Unique keys; nothing deleted directly (the chained-tables rule —
// the reap function is the subject under test).
func TestConversationTurnRoundTripExpiryAndReap(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to an empty database to run the conversation-store round-trip")
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
	s := NewConversationStore(p)
	key := fmt.Sprintf("nginxdown|p28-web-%d", time.Now().UnixNano())

	if err := s.Append(ctx, key, "TG-t1", "outcome=stop — first", time.Hour); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Append(ctx, key, "TG-t2", "outcome=proposed — second", time.Hour); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Append(ctx, "", "TG-t3", "unkeyed", time.Hour); err == nil {
		t.Fatal("an unkeyed append must refuse")
	}

	turns, err := s.Recent(ctx, key, "TG-asking", 5)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(turns) != 2 || turns[0].ExternalRef != "TG-t2" || turns[1].ExternalRef != "TG-t1" {
		t.Fatalf("newest-first read-back broke: %+v", turns)
	}
	// The asking session never reads itself (a retried terminal must not echo).
	turns, err = s.Recent(ctx, key, "TG-t2", 5)
	if err != nil || len(turns) != 1 || turns[0].ExternalRef != "TG-t1" {
		t.Fatalf("self-exclusion broke: %+v err=%v", turns, err)
	}

	// Expire one turn (an UPDATE on this test's own row via SQL — the store exposes no such lever, and
	// that absence is the point) and prove Recent hides it while the DEFINER reap removes it.
	if _, err := p.Exec(ctx,
		`UPDATE conversation_turn SET expires_at = now() - interval '1 minute' WHERE conversation_key = $1 AND external_ref = 'TG-t1'`,
		key); err != nil {
		t.Fatalf("expire: %v", err)
	}
	turns, err = s.Recent(ctx, key, "TG-asking", 5)
	if err != nil || len(turns) != 1 || turns[0].ExternalRef != "TG-t2" {
		t.Fatalf("an expired turn must be invisible: %+v err=%v", turns, err)
	}
	n, err := s.Reap(ctx, 100)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n < 1 {
		t.Fatalf("the reap must remove at least this test's expired turn, got %d", n)
	}
	var live int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM conversation_turn WHERE conversation_key = $1`, key).Scan(&live); err != nil {
		t.Fatalf("count: %v", err)
	}
	if live != 1 {
		t.Fatalf("exactly the live turn must remain, got %d", live)
	}
}
