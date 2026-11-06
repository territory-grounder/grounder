package db

import (
	"context"
	"os"
	"testing"
)

// TestEstateObjectGroupStoreRoundTrip exercises the object-group store against a real migrated Postgres
// (TG-481). It skips silently without TG_TEST_POSTGRES_DSN and self-migrates, mirroring the native-rule
// store test — a pgx fake would hide exactly the text[] round-trip and the RETURNING/RowsAffected semantics
// this asserts.
func TestEstateObjectGroupStoreRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the estate-object-group store test")
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
	defer func() {
		if _, err := p.Pool.Exec(ctx, `DELETE FROM estate_object_group`); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	store := NewEstateObjectGroupStore(p)

	// empty to start.
	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty store, got %d rows", len(got))
	}

	// insert one group with a multi-element text[] of patterns.
	id, err := store.Insert(ctx, "edge-firewalls", []string{"dc1demo-fw*", "dc2demo-fw*"}, "union", "operator:test")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id == 0 {
		t.Fatalf("insert returned id 0")
	}

	// round-trip: every field survives, and the text[] comes back in order.
	got, err = store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 row, got %d", len(got))
	}
	r := got[0]
	if r.ID != id {
		t.Errorf("id: got %d, want %d", r.ID, id)
	}
	if r.Name != "edge-firewalls" {
		t.Errorf("name: got %q", r.Name)
	}
	if len(r.Patterns) != 2 || r.Patterns[0] != "dc1demo-fw*" || r.Patterns[1] != "dc2demo-fw*" {
		t.Errorf("patterns round-trip failed: got %v", r.Patterns)
	}
	if r.Precedence != "union" {
		t.Errorf("precedence: got %q, want union", r.Precedence)
	}
	if r.CreatedBy != "operator:test" {
		t.Errorf("created_by: got %q", r.CreatedBy)
	}
	if r.CreatedAt.IsZero() {
		t.Errorf("created_at was zero")
	}

	// delete reports found, then not-found on the second call.
	ok, err := store.Delete(ctx, id)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !ok {
		t.Fatalf("delete reported not-found for an existing row")
	}
	ok, err = store.Delete(ctx, id)
	if err != nil {
		t.Fatalf("delete (second): %v", err)
	}
	if ok {
		t.Fatalf("second delete should report not-found")
	}
}
