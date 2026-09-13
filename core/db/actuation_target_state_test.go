package db

import (
	"context"
	"os"
	"strings"
	"testing"
)

func targetAdmissionFixture(t *testing.T) (context.Context, *Pool, *ActuationTargetStore) {
	t.Helper()
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to an empty database to run the target-admission round-trip")
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
	return ctx, p, NewActuationTargetStore(p)
}

// TG-81 b2: the whole admission contract against a REAL Postgres — a fake would hide the conditional
// upsert this control IS. Unique target names per case; nothing deleted (the chained-tables rule).
func TestTargetAdmissionClaimConflictAndRelease(t *testing.T) {
	ctx, _, s := targetAdmissionFixture(t)
	const target = "b2-admission-web01"

	if err := s.Admit(ctx, target, "TG-a"); err != nil {
		t.Fatalf("first claim must admit: %v", err)
	}
	// A second hand — any process — is refused while the claim stands, and the refusal names the holder.
	err := s.Admit(ctx, target, "TG-b")
	if err == nil || !strings.Contains(err.Error(), "TG-a") {
		t.Fatalf("a held target must refuse naming the holder, got %v", err)
	}
	// An UNDISTURBED release frees the target immediately — no cooldown for a healthy heal.
	s.Release(ctx, target, "TG-a", false)
	if err := s.Admit(ctx, target, "TG-b"); err != nil {
		t.Fatalf("released target must admit the next hand: %v", err)
	}
	s.Release(ctx, target, "TG-b", false)
}

func TestTargetAdmissionCooldownParksTheTarget(t *testing.T) {
	ctx, p, s := targetAdmissionFixture(t)
	const target = "b2-admission-db02"

	if err := s.Admit(ctx, target, "TG-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// A DISTURBED release (failed/killed effect) parks the target: the next hand is refused with the
	// cooldown reason even though nothing holds a claim.
	s.Release(ctx, target, "TG-a", true)
	err := s.Admit(ctx, target, "TG-b")
	if err == nil || !strings.Contains(err.Error(), "cooling down") {
		t.Fatalf("a disturbed target must refuse with the cooldown reason, got %v", err)
	}
	// Expire the window (an UPDATE on this test's own unique row, not a wait) — admission must recover:
	// the cooldown is the refusing condition, not a permanently parked row.
	if _, uerr := p.Exec(ctx,
		`UPDATE actuation_target_state SET cooldown_until = now() - interval '1 second' WHERE target = $1`,
		target); uerr != nil {
		t.Fatalf("expire cooldown: %v", uerr)
	}
	if err := s.Admit(ctx, target, "TG-b"); err != nil {
		t.Fatalf("an expired cooldown must admit: %v", err)
	}
	s.Release(ctx, target, "TG-b", false)
}

// A crashed worker's claim must age out (claim TTL takeover) — otherwise one crash parks a target forever
// with no operator lever; and a RELEASE scoped to the wrong ref must not clear a newer claim.
func TestTargetAdmissionStaleTakeoverAndScopedRelease(t *testing.T) {
	ctx, p, s := targetAdmissionFixture(t)
	const target = "b2-admission-pve03"

	if err := s.Admit(ctx, target, "TG-crashed"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Age the claim past the TTL (own unique row).
	if _, err := p.Exec(ctx,
		`UPDATE actuation_target_state SET claimed_at = now() - interval '11 minutes' WHERE target = $1`,
		target); err != nil {
		t.Fatalf("age claim: %v", err)
	}
	if err := s.Admit(ctx, target, "TG-next"); err != nil {
		t.Fatalf("a stale claim must be taken over: %v", err)
	}
	// The crashed session's late release must NOT clear the new claim (ref-scoped release).
	s.Release(ctx, target, "TG-crashed", false)
	err := s.Admit(ctx, target, "TG-third")
	if err == nil || !strings.Contains(err.Error(), "TG-next") {
		t.Fatalf("a stale release must not clear the live claim, got %v", err)
	}
	s.Release(ctx, target, "TG-next", false)
}

// An empty target shares one estate-wide bucket rather than bypassing admission — the no-exemption
// posture the in-process limiter set.
func TestTargetAdmissionEmptyTargetIsNotAnExemption(t *testing.T) {
	ctx, p, s := targetAdmissionFixture(t)
	// The (no-target) bucket is SHARED across the whole suite by design; reset ITS row to a known state
	// (an UPDATE on the shared row, never a delete — the chained-tables rule) so this oracle is
	// deterministic instead of skipping, which the dsn-gate meta-test rightly refuses.
	if _, err := p.Exec(ctx,
		`UPDATE actuation_target_state SET claimed_by = '', claimed_at = NULL, cooldown_until = NULL WHERE target = '(no-target)'`); err != nil {
		t.Fatalf("reset shared bucket: %v", err)
	}
	if err := s.Admit(ctx, "  ", "TG-a"); err != nil {
		t.Fatalf("the reset shared bucket must admit the first unkeyed hand: %v", err)
	}
	if err := s.Admit(ctx, "", "TG-b"); err == nil {
		t.Fatal("two unkeyed actuations must contend for ONE bucket, not bypass admission")
	}
	s.Release(ctx, "", "TG-a", false)
}
