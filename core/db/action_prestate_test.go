package db

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/actuate"
)

func preStateFixture(t *testing.T) (context.Context, *Pool) {
	t.Helper()
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to an empty database to run the pre-state store round-trip")
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
	return ctx, p
}

// TG-58: the pre-state store round-trips a captured snapshot bound to an action_id through a REAL Postgres (a
// pgx fake would hide the ON CONFLICT upsert and the bytea round-trip), and re-capturing the same action_id
// OVERWRITES it (latest-wins) — matching an undo targeting the most recent execution of a shape.
func TestActionPreStateStore_RoundTripAndLatestWins(t *testing.T) {
	ctx, p := preStateFixture(t)
	s := NewActionPreStateStore(p)
	const aid = "prestate-roundtrip-action"
	// A shared fixture DB may carry a prior row — start from a known-clean slate for this id.
	if _, err := p.Exec(ctx, `DELETE FROM action_prestate WHERE action_id = $1`, aid); err != nil {
		t.Fatalf("pre-clean: %v", err)
	}

	if err := s.RecordPreState(ctx, aid, actuate.PreState{Kind: "service", Data: []byte("nginx=active,enabled")}); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, found, err := s.PreStateFor(ctx, aid)
	if err != nil || !found {
		t.Fatalf("read back: found=%v err=%v", found, err)
	}
	if got.Kind != "service" || !bytes.Equal(got.Data, []byte("nginx=active,enabled")) {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// LATEST-WINS: re-capturing the same action_id overwrites the prior snapshot.
	if err := s.RecordPreState(ctx, aid, actuate.PreState{Kind: "service", Data: []byte("nginx=inactive,disabled")}); err != nil {
		t.Fatalf("re-record: %v", err)
	}
	got2, _, err := s.PreStateFor(ctx, aid)
	if err != nil {
		t.Fatalf("read back 2: %v", err)
	}
	if !bytes.Equal(got2.Data, []byte("nginx=inactive,disabled")) {
		t.Fatalf("latest-wins failed: got %q, want the overwritten value", got2.Data)
	}

	// An action with no captured pre-state is a clean not-found, distinct from an error.
	if _, found, err := s.PreStateFor(ctx, "prestate-no-such-action"); err != nil || found {
		t.Fatalf("unknown action must be not-found: found=%v err=%v", found, err)
	}

	// Empty action_id is refused, not silently written.
	if err := s.RecordPreState(ctx, "  ", actuate.PreState{Kind: "x", Data: []byte("y")}); err == nil {
		t.Fatal("RecordPreState must reject a blank action_id")
	}
}
