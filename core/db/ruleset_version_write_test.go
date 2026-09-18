package db

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
)

// jsonEqual compares two documents by their PARSED JSON, not their bytes — the archive round-trips through a
// jsonb column, so Postgres normalizes whitespace + key order; only the document's meaning is guaranteed stable.
func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// TestRulesetVersionRoundTrip drives the REAL pgx path (INSERT ... ON CONFLICT DO NOTHING -> SELECT) for the
// immutable versioned-ruleset archive (spec/020 T-020-6, REQ-2018): distinct bundle_versions resolve to their
// exact documents, an unknown version is a clean miss, and a re-Save of an existing version is a no-op
// (first-wins immutability) — a field the SQL forgot to carry, or an accidental overwrite, fails HERE.
// Gated on TG_TEST_POSTGRES_DSN (CI has no Postgres).
func TestRulesetVersionRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the ruleset-version round-trip test")
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
	s := NewRulesetVersionStore(p)

	uniq := fmt.Sprintf("rv-it-%d", os.Getpid())
	vA, vB := uniq+"-a", uniq+"-b"
	docA := []byte(`{"rules":[{"id":"a","verdict":"deny","match":{"argv_pattern":"rm -rf /"}}]}`)
	docB := []byte(`{"rules":[{"id":"b","verdict":"auto","match":{"op_class":"restart-service"}}]}`)
	defer func() {
		// Cleanup requires the owner role in prod (0029 REVOKEs DELETE from tg_runtime); best-effort under test.
		_, _ = p.Exec(ctx, "DELETE FROM policy_ruleset_version WHERE bundle_version = ANY($1)", []string{vA, vB})
	}()

	if err := s.Save(ctx, vA, docA, 1, "op-a"); err != nil {
		t.Fatalf("save vA: %v", err)
	}
	if err := s.Save(ctx, vB, docB, 1, "op-b"); err != nil {
		t.Fatalf("save vB: %v", err)
	}

	gotA, ok, err := s.Get(ctx, vA)
	if err != nil || !ok || !jsonEqual(gotA, docA) {
		t.Fatalf("get vA = (%s, ok=%v, %v), want docA — the version archive dropped/mismatched the doc", gotA, ok, err)
	}
	gotB, ok, err := s.Get(ctx, vB)
	if err != nil || !ok || !jsonEqual(gotB, docB) {
		t.Fatalf("get vB = (%s, ok=%v, %v), want docB", gotB, ok, err)
	}

	// Unknown version → clean miss (never the current singleton).
	if _, ok, err := s.Get(ctx, uniq+"-missing"); err != nil || ok {
		t.Fatalf("get missing = (ok=%v, %v), want (false, nil)", ok, err)
	}

	// First-wins immutability: re-Saving vA with a DIFFERENT doc is a no-op (ON CONFLICT DO NOTHING).
	if err := s.Save(ctx, vA, docB, 1, "attacker"); err != nil {
		t.Fatalf("re-save vA: %v", err)
	}
	gotA2, ok, err := s.Get(ctx, vA)
	if err != nil || !ok || !jsonEqual(gotA2, docA) {
		t.Fatalf("after re-save, get vA = (%s, ok=%v, %v), want the ORIGINAL docA — immutability broken", gotA2, ok, err)
	}
}
