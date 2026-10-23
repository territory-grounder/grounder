package persist

import (
	"context"
	"testing"
	"time"
)

func mustP(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func openRefs(ds []PendingDecision) []string {
	r := make([]string, len(ds))
	for i, d := range ds {
		r[i] = d.ExternalRef
	}
	return r
}

// The in-memory pending-decision projection's full lifecycle: open (fail-closed on a missing key, Band
// forced to POLL_PAUSE), oldest-first listing, resolve, and — the governance-critical property — a resolve
// with a MISMATCHED actionID is a no-op (a vote/timeout for a different action must never release this one).
func TestMemPendingDecisionsLifecycle(t *testing.T) {
	ctx := context.Background()
	m := NewMemPendingDecisions()

	if n, _ := m.CountOpen(ctx); n != 0 {
		t.Fatalf("empty CountOpen=%d want 0", n)
	}

	// fail closed on a decision with no correlation key or no bound action.
	if err := m.OpenDecision(ctx, PendingDecision{ActionID: "a1"}); err != ErrEmptyDecisionKey {
		t.Fatalf("missing external_ref: err=%v want ErrEmptyDecisionKey", err)
	}
	if err := m.OpenDecision(ctx, PendingDecision{ExternalRef: "r1"}); err != ErrEmptyDecisionKey {
		t.Fatalf("missing action_id: err=%v want ErrEmptyDecisionKey", err)
	}

	// open two, out of order, with a caller-set Band that must be forced to POLL_PAUSE.
	t0 := time.Now()
	mustP(t, m.OpenDecision(ctx, PendingDecision{ExternalRef: "r2", ActionID: "a2", OpenedAt: t0.Add(time.Second), Band: "AUTO"}))
	mustP(t, m.OpenDecision(ctx, PendingDecision{ExternalRef: "r1", ActionID: "a1", OpenedAt: t0}))

	if n, _ := m.CountOpen(ctx); n != 2 {
		t.Fatalf("CountOpen=%d want 2", n)
	}
	open, _ := m.OpenDecisions(ctx)
	if got := openRefs(open); len(got) != 2 || got[0] != "r1" || got[1] != "r2" {
		t.Fatalf("OpenDecisions oldest-first wrong: %v", got)
	}
	if open[0].Band != "POLL_PAUSE" {
		t.Fatalf("Band not forced to POLL_PAUSE, got %q", open[0].Band)
	}

	// GOVERNANCE-CRITICAL: a resolve for a DIFFERENT action must not release r1.
	mustP(t, m.ResolveDecision(ctx, "r1", "WRONG-action", "approved", t0))
	if n, _ := m.CountOpen(ctx); n != 2 {
		t.Fatalf("resolve with a mismatched actionID must be a no-op; CountOpen=%d want 2", n)
	}

	// resolve r1 with its bound action → it leaves the open set.
	rt := t0.Add(time.Minute)
	mustP(t, m.ResolveDecision(ctx, "r1", "a1", "approved", rt))
	if n, _ := m.CountOpen(ctx); n != 1 {
		t.Fatalf("after resolve CountOpen=%d want 1", n)
	}
	if got := openRefs(mustOpen(t, m, ctx)); len(got) != 1 || got[0] != "r2" {
		t.Fatalf("only r2 should remain open, got %v", got)
	}

	// idempotent: an unknown ref and an already-resolved ref are no-ops, never errors.
	mustP(t, m.ResolveDecision(ctx, "does-not-exist", "a", "denied", rt))
	mustP(t, m.ResolveDecision(ctx, "r1", "a1", "denied", rt))
	if n, _ := m.CountOpen(ctx); n != 1 {
		t.Fatalf("idempotent resolves changed the count: %d want 1", n)
	}

	// re-opening a resolved ref (upsert) puts it back to open.
	mustP(t, m.OpenDecision(ctx, PendingDecision{ExternalRef: "r1", ActionID: "a1b", OpenedAt: t0}))
	if n, _ := m.CountOpen(ctx); n != 2 {
		t.Fatalf("re-open CountOpen=%d want 2", n)
	}
}

func mustOpen(t *testing.T, m *MemPendingDecisions, ctx context.Context) []PendingDecision {
	t.Helper()
	ds, err := m.OpenDecisions(ctx)
	mustP(t, err)
	return ds
}

func TestPendingStatusString(t *testing.T) {
	if DecisionOpen.String() != "open" || DecisionResolved.String() != "resolved" {
		t.Fatalf("PendingStatus.String: open=%q resolved=%q", DecisionOpen.String(), DecisionResolved.String())
	}
}
