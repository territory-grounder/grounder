package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestADispatchIsClaimedOnceAndARepeatHealIsNot(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to an empty database to run the dispatch claim")
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
	s := NewActuationDispatchStore(p)
	run := fmt.Sprintf("tg692-%d", time.Now().UnixNano())
	first, err := s.ClaimDispatch(ctx, run+"/1", "action-shape", "incident-a")
	if err != nil || !first {
		t.Fatalf("the first claim must win: %v %v", first, err)
	}
	again, err := s.ClaimDispatch(ctx, run+"/1", "action-shape", "incident-a")
	if err != nil || again {
		t.Fatalf("a same-dispatch retry must not win: %v %v", again, err)
	}
	other, err := s.ClaimDispatch(ctx, run+"/2", "action-shape", "incident-b")
	if err != nil || !other {
		t.Fatalf("a new dispatch of the same action shape must be admitted: %v %v", other, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if won, err := s.ClaimDispatch(cancelled, run+"/3", "a", "b"); err == nil || won {
		t.Fatalf("an unreadable store must return an error, got %v %v", won, err)
	}
}
