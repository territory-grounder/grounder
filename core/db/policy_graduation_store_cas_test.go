package db

// TG-146 S3/S4 — the durable policy_graduation store's optimistic-concurrency guard, against real Postgres
// (migration 0101 + the guarded upsert). Only a live DB proves the `ON CONFLICT ... WHERE version = $expected
// RETURNING version` shape and its pgx.ErrNoRows-means-lost mapping; the in-memory oracle in
// core/policy/graduation_cas_test.go pins the same contract for CI (no DB there). This mirrors
// core/db/breaker_compare_and_open_test.go, the durable-breaker CompareAndOpen pattern this guard copies.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/policy"
)

func gradCASDB(t *testing.T) (*Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the graduation CAS oracle")
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
	if _, err := p.Pool.Exec(ctx, "DELETE FROM policy_graduation WHERE op_class LIKE 'tg146-%'"); err != nil {
		t.Fatalf("clean fixture: %v", err)
	}
	return p, ctx
}

// TestPolicyGraduationStoreCASRejectsAStaleCrossProcessWrite drives the real guarded upsert: two independent
// store instances (two workers over the same DB) read the same row; the first write lands, and the second —
// still holding the pre-write version — is refused with policy.ErrConcurrentModification, never a blind clobber
// that resurrects withdrawn autonomy. It is the Postgres proof of the S3/S4 fix (the per-instance mutex cannot
// serialize across processes; the version guard is what does).
func TestPolicyGraduationStoreCASRejectsAStaleCrossProcessWrite(t *testing.T) {
	p, ctx := gradCASDB(t)
	const op = "tg146-cas-demote"
	workerA := NewPolicyGraduationStore(p)
	workerB := NewPolicyGraduationStore(p)

	// Seed a durable row via a fresh (version-0) UNCONDITIONAL insert. approve (rank 1) needs no graduation
	// credit — the 0067 trigger only guards an ADVANCEMENT to an autonomous rank — so the CAS semantics under
	// test are isolated from the promotion-grounding trigger.
	if err := workerA.Save(ctx, policy.ClassState{OpClass: op, Level: policy.LevelApprove, CleanRunCount: 3}); err != nil {
		t.Fatalf("seed insert: %v", err)
	}

	a, err := workerA.Load(ctx, op)
	if err != nil {
		t.Fatalf("A load: %v", err)
	}
	b, err := workerB.Load(ctx, op)
	if err != nil {
		t.Fatalf("B load: %v", err)
	}
	if a.Version < 1 || a.Version != b.Version {
		t.Fatalf("both workers must read the same persisted version >= 1: a=%d b=%d", a.Version, b.Version)
	}

	// Worker A commits first (same level, streak reset → no trigger involvement); its version bumps.
	a.CleanRunCount = 0
	if err := workerA.Save(ctx, a); err != nil {
		t.Fatalf("A guarded save: %v", err)
	}

	// Worker B, still holding the pre-A version, tries to write its stale state — MUST be refused.
	b.CleanRunCount = 99
	if err := workerB.Save(ctx, b); !errors.Is(err, policy.ErrConcurrentModification) {
		t.Fatalf("stale cross-process B save = %v, want policy.ErrConcurrentModification (the clobber the guard stops)", err)
	}

	// The durable row is A's write (count 0), not B's clobber (count 99), and the version advanced past A's read.
	final, err := workerA.Load(ctx, op)
	if err != nil {
		t.Fatalf("final load: %v", err)
	}
	if final.CleanRunCount != 0 {
		t.Fatalf("durable clean_run_count = %d, want 0 (A's write survived, B's stale clobber refused)", final.CleanRunCount)
	}
	if final.Version <= a.Version {
		t.Fatalf("version did not advance past A's read: final=%d a=%d", final.Version, a.Version)
	}

	// An UNCONDITIONAL (version 0) write — the ratify verb's authoritative reset shape — always lands, whatever
	// the row currently holds; it is never a CAS.
	if err := workerB.Save(ctx, policy.ClassState{OpClass: op, Level: policy.LevelApprove}); err != nil {
		t.Fatalf("unconditional version-0 reset = %v, want nil (an authoritative reset must always win)", err)
	}
}
