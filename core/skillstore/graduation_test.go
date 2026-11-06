package skillstore

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

// graduated builds a store with v1 retired-by-supersede and v2 production (the post-graduation shape).
func graduated(t *testing.T) (*MemStore, *audit.Ledger, Version, Version) {
	t.Helper()
	m := NewMemStore()
	m.PutSkill(Skill{Name: "triage-protocol", Kind: "behavioral", Position: 5})
	lg := audit.NewLedger()
	ctx := context.Background()
	v1 := draft(t, m, "triage-protocol", "1.0.0", "body v1")
	mustTransition := func(id int64, to Status, why string) Version {
		v, err := Transition(ctx, m, lg, id, to, why)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	mustTransition(v1.ID, StatusTrial, "gate")
	v1 = mustTransition(v1.ID, StatusProduction, "initial")
	v2 := draft(t, m, "triage-protocol", "2.0.0", "body v2 (graduate)")
	mustTransition(v2.ID, StatusTrial, "gate")
	v2 = mustTransition(v2.ID, StatusProduction, "welch win")
	return m, lg, v1, v2
}

// REQ-1310: five consecutive regressing sessions trip the watch — the graduate retires, the prior
// body returns as the production version, the demotion is ledgered and escalated.
func TestWatchTripDemotesAndRestores(t *testing.T) {
	m, lg, v1, v2 := graduated(t)
	ws := NewMemWatchStore()
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	if err := OpenWatch(ctx, ws, v2.ID, v1.ID, "triage-protocol", "correct_diagnosis", 3.5, 0.05, now); err != nil {
		t.Fatal(err)
	}
	var escalated string
	esc := func(_ context.Context, ref, reason string) error { escalated = ref + ": " + reason; return nil }
	dim := func(v float64) map[string]float64 { return map[string]float64{"correct_diagnosis": v} }

	// Four failures + one recovery resets the streak — no trip.
	for i := 0; i < 4; i++ {
		if err := ObserveJudgedSession(ctx, ws, m, lg, esc, []int64{v2.ID}, dim(2.0), now); err != nil {
			t.Fatal(err)
		}
	}
	if err := ObserveJudgedSession(ctx, ws, m, lg, esc, []int64{v2.ID}, dim(4.0), now); err != nil {
		t.Fatal(err)
	}
	// A regressing score on a DIFFERENT dimension is no evidence for this watch — the streak holds.
	if err := ObserveJudgedSession(ctx, ws, m, lg, esc, []int64{v2.ID}, map[string]float64{"appropriate_band": 1.0}, now); err != nil {
		t.Fatal(err)
	}
	// Five consecutive failures trip.
	for i := 0; i < 5; i++ {
		if err := ObserveJudgedSession(ctx, ws, m, lg, esc, []int64{v2.ID}, dim(2.0), now); err != nil {
			t.Fatal(err)
		}
	}

	got, _ := m.GetVersion(ctx, v2.ID)
	if got.Status != StatusRetired {
		t.Fatalf("the tripped graduate must retire, got %s", got.Status)
	}
	prod, ok, _ := m.ProductionVersion(ctx, "triage-protocol")
	if !ok || prod.Body != "body v1" {
		t.Fatalf("the prior body must be production again, got %+v", prod)
	}
	if !strings.Contains(prod.Version, "restored") || prod.ParentVersionID != v1.ID {
		t.Fatalf("the restore must be a new lineage-linked version, got %+v", prod)
	}
	if escalated == "" || !strings.Contains(escalated, "regression watch tripped") {
		t.Fatalf("the demotion must escalate, got %q", escalated)
	}
	if err := lg.Verify(); err != nil {
		t.Fatalf("ledger chain must verify after the demotion: %v", err)
	}
}

// A session that did not compose the watched version never counts against it, and an expired watch
// closes as survived.
func TestWatchScopingAndExpiry(t *testing.T) {
	m, lg, v1, v2 := graduated(t)
	ws := NewMemWatchStore()
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	if err := OpenWatch(ctx, ws, v2.ID, v1.ID, "triage-protocol", "correct_diagnosis", 3.5, 0.05, now); err != nil {
		t.Fatal(err)
	}
	low := map[string]float64{"correct_diagnosis": 1.0}
	for i := 0; i < 10; i++ {
		if err := ObserveJudgedSession(ctx, ws, m, lg, nil, []int64{999}, low, now); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := m.GetVersion(ctx, v2.ID); got.Status != StatusProduction {
		t.Fatalf("unrelated sessions must not demote, got %s", got.Status)
	}
	if err := ObserveJudgedSession(ctx, ws, m, lg, nil, []int64{v2.ID}, low, now.Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if reason := ws.closed[v2.ID]; !strings.Contains(reason, "survived") {
		t.Fatalf("an expired watch closes as survived, got %q", reason)
	}
}
