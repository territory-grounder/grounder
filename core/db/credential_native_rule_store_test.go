package db

import (
	"context"
	"os"
	"testing"
)

// TestCredentialNativeRuleStoreRoundTrip is the DB-gated proof for the operator-authored native credential
// mapping's store (TG-109, migration 0088): an inserted rule row lists back verbatim in id order, a delete
// removes exactly its row, and a delete of a MISSING row reports (false, nil) — the fact the worker lane
// turns into its typed ErrNoSuchRule (→ 404 at the surface) rather than a phantom success. Runs only with
// a migrated Postgres (self-migrating, the TestCoOccurrenceStoreRoundTrip pattern).
func TestCredentialNativeRuleStoreRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the native-rule store test")
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
		if _, err := p.Exec(ctx, `DELETE FROM credential_native_rule`); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()

	store := NewCredentialNativeRuleStore(p)

	// Insert → List round-trip: both rows come back verbatim, in id (insertion) order. The entries carry
	// SecretRef REFERENCES only — exactly what the write lane admits (INV-13).
	e1 := "host-glob:web-*|deploy|22|ssh|env:TG_TEST_KEY_A"
	e2 := "host:db01|postgres|22|ssh|store:db01.key"
	id1, err := store.Insert(ctx, e1, "cover the web tier", "operator:kyriakosp")
	if err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	id2, err := store.Insert(ctx, e2, "db01 maintenance identity", "operator:kyriakosp")
	if err != nil {
		t.Fatalf("insert 2: %v", err)
	}
	if id2 <= id1 {
		t.Fatalf("ids must be monotonic: id1=%d id2=%d", id1, id2)
	}
	rows, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("list returned %d rows, want 2", len(rows))
	}
	if rows[0].ID != id1 || rows[0].Entry != e1 || rows[0].Rationale != "cover the web tier" || rows[0].CreatedBy != "operator:kyriakosp" {
		t.Fatalf("row 1 did not round-trip: %+v", rows[0])
	}
	if rows[1].ID != id2 || rows[1].Entry != e2 {
		t.Fatalf("row 2 did not round-trip: %+v", rows[1])
	}
	if rows[0].CreatedAt.IsZero() || rows[1].CreatedAt.IsZero() {
		t.Fatalf("created_at must be stamped by the database, got zero times: %v / %v", rows[0].CreatedAt, rows[1].CreatedAt)
	}

	// Delete removes exactly its row and reports true.
	ok, err := store.Delete(ctx, id1)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !ok {
		t.Fatalf("delete of an existing row reported false")
	}
	rows, err = store.List(ctx)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != id2 {
		t.Fatalf("after deleting id %d the list is %+v, want only id %d", id1, rows, id2)
	}

	// Delete of a MISSING row is (false, nil) — a fact, not an error; the worker lane owns the refusal.
	ok, err = store.Delete(ctx, id1)
	if err != nil {
		t.Fatalf("delete missing: unexpected error %v", err)
	}
	if ok {
		t.Fatalf("delete of a missing row reported true — the 404 mapping would lie")
	}

	// The schema itself refuses a blank entry/rationale (CHECK btrim) — defense in depth under the worker
	// lane's ParseRules validation, so even a bypassed writer cannot store an unparseable blank.
	if _, err := store.Insert(ctx, "   ", "r", "op"); err == nil {
		t.Fatalf("blank entry was accepted — the CHECK constraint is not enforcing")
	}
	if _, err := store.Insert(ctx, "host:h|u|22|ssh|env:K", "   ", "op"); err == nil {
		t.Fatalf("blank rationale was accepted — the CHECK constraint is not enforcing")
	}
}
