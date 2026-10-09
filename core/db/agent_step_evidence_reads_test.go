package db

import (
	"testing"
	"time"
)

// THE SEED READ FOR THE RECON BUDGET (TG-165).
//
// The read-lane meter lives in core/safety and counts in process memory, which means a RESTART would hand
// whatever was mid-burst a brand-new hour — and "restart the worker" is not a step an intruder finds
// difficult. agent_step_evidence is the durable record of what was read (TG-295 bounded it; TG-272 created
// it), so it is what the rolling hour is seeded from. This oracle is about the query being right: the
// WINDOW must be respected, and rows outside it must never be adopted.
//
// KILLING MUTATION: change the predicate to `created_at >= $1 - interval '2 hours'` (or drop the WHERE).
// RED — a booting worker adopts hours of old reads as if they had just happened and refuses the first real
// investigation of the day.
func TestReadsSinceReturnsOnlyTheRequestedWindow(t *testing.T) {
	ctx, p, ref := evidenceReapFixture(t)
	now := time.Now().UTC()
	cleanupEvidence(t, ctx, p, ref, now)
	s := NewAgentStepEvidenceStore(p)

	// Three reads inside the last hour, two well outside it.
	inside := []time.Time{now.Add(-5 * time.Minute), now.Add(-20 * time.Minute), now.Add(-55 * time.Minute)}
	outside := []time.Time{now.Add(-70 * time.Minute), now.Add(-26 * time.Hour)}
	for i, at := range append(append([]time.Time{}, inside...), outside...) {
		seedEvidence(t, ctx, p, ref, i+1, "ev-reads-"+time.Duration(i).String(), at)
	}

	got, err := s.ReadsSince(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("ReadsSince: %v", err)
	}
	if len(got) != len(inside) {
		t.Fatalf("want %d reads inside the rolling hour, got %d (%v) — the seed decides how much budget a "+
			"freshly booted worker starts with, so a wrong window either blinds triage or grants a free hour",
			len(inside), len(got), got)
	}
	// Oldest first, and every row genuinely inside the window.
	for i, at := range got {
		if at.Before(now.Add(-time.Hour)) {
			t.Errorf("row %d (%s) is older than the requested window", i, at)
		}
		if i > 0 && at.Before(got[i-1]) {
			t.Errorf("rows must come back oldest-first; %s precedes %s", got[i-1], at)
		}
	}
	// Vacuity floor: a query that returns nothing would pass every assertion above.
	if len(got) == 0 {
		t.Fatal("vacuity floor: ReadsSince returned nothing, so this oracle proved nothing about the window")
	}
	// And a window with nothing in it returns nothing — the fresh-install case, which must bind no budget.
	if none, err := s.ReadsSince(ctx, now.Add(time.Hour)); err != nil || len(none) != 0 {
		t.Fatalf("a future window must be empty (fresh install binds nothing): n=%d err=%v", len(none), err)
	}
}
