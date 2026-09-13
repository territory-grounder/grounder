package db

import (
	"context"
	"os"
	"testing"
)

// TestPolicyEngineToggleStoreRoundTrip drives the REAL pgx path for the durable admin engine-toggle override
// (TG-506, migration 0030): an empty table reads no override (nil); a saved true/false round-trips; and a nil
// Save clears it back to no-override. The NULL-vs-false distinction is exactly what a pgx fake cannot model —
// a fake that returned a zero-value bool would read a cleared override as "engine disabled", which on the
// worker plane is a permissive-posture flip. Gated on TG_TEST_POSTGRES_DSN (an EMPTY db; it Migrates itself).
func TestPolicyEngineToggleStoreRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to an empty database to run the engine-toggle store round-trip")
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
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM policy_engine_toggle") }()

	s := NewPolicyEngineToggleStore(p)

	// Empty table ⇒ no override (follow the per-mode default).
	if ov, err := s.Load(ctx); err != nil || ov != nil {
		t.Fatalf("empty store must Load (nil, nil), got (%v, %v)", ov, err)
	}
	// Save true ⇒ Load true.
	tru := true
	if err := s.Save(ctx, &tru, "admin-a"); err != nil {
		t.Fatalf("save true: %v", err)
	}
	if ov, err := s.Load(ctx); err != nil || ov == nil || *ov != true {
		t.Fatalf("after save true, Load must be &true, got (%v, %v)", ov, err)
	}
	// Save false ⇒ Load false — distinct from NULL (the field the fake would drop).
	fls := false
	if err := s.Save(ctx, &fls, "admin-b"); err != nil {
		t.Fatalf("save false: %v", err)
	}
	if ov, err := s.Load(ctx); err != nil || ov == nil || *ov != false {
		t.Fatalf("after save false, Load must be &false (not NULL), got (%v, %v)", ov, err)
	}
	// Save nil ⇒ cleared to SQL NULL ⇒ Load nil.
	if err := s.Save(ctx, nil, "admin-c"); err != nil {
		t.Fatalf("save nil: %v", err)
	}
	if ov, err := s.Load(ctx); err != nil || ov != nil {
		t.Fatalf("after save nil (clear), Load must be (nil, nil), got (%v, %v)", ov, err)
	}
}
