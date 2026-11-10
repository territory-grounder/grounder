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
// body returns as a TRIAL candidate (TG-673), the demotion is ledgered and escalated.
func TestWatchTripDemotesAndRestores(t *testing.T) {
	m, lg, v1, v2 := graduated(t)
	ws := NewMemWatchStore()
	ctx := context.Background()
	// Control: before the trip the composer's one-read snapshot carries the graduate.
	if rows, err := m.ProductionRows(ctx); err != nil || len(rows) != 1 || rows[0].VersionID != v2.ID {
		t.Fatalf("control: the graduate must be the skill's production row, got %+v %v", rows, err)
	}
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
	// TG-673 (deep-review F15): the restore lands in TRIAL, never straight back in production — a body nobody
	// re-judged must not go live under a graduation-shaped audit trail. KILLING MUTATION: re-add the
	// Trial → Production transition in restorePrior and both assertions below fail.
	if _, ok, _ := m.ProductionVersion(ctx, "triage-protocol"); ok {
		t.Fatal("after the watch trips there must be NO production version — the skill composes from the compiled registry until a trial or an operator promotes the restore")
	}
	all := m.VersionsOf("triage-protocol")
	var restored Version
	for _, v := range all {
		if strings.Contains(v.Version, "restored") {
			restored = v
		}
	}
	if restored.ID == 0 || restored.Status != StatusTrial || restored.Body != "body v1" || restored.ParentVersionID != v1.ID {
		t.Fatalf("the prior body must return as a lineage-linked TRIAL candidate, got %+v", restored)
	}
	if escalated == "" || !strings.Contains(escalated, "regression watch tripped") || !strings.Contains(escalated, "TRIAL") ||
		!strings.Contains(escalated, "ONLY an operator promotion") {
		t.Fatalf("the demotion must escalate, say the body is back in TRIAL and that only an operator promotion returns it, got %q", escalated)
	}
	// Composition after the trip (REQ-1304's fallback): the snapshot the composer takes carries NO row for the
	// skill, so agent/skills falls back to the compiled registry for it — and nothing but an operator promotion
	// changes that, because the flywheel admits trial candidates only under a production parent
	// (AdmittedCandidates), which this skill no longer has. The restored row is not such a candidate.
	rows, err := m.ProductionRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.SkillName == "triage-protocol" {
			t.Fatalf("after the trip the composer must see NO production row for the skill, got %+v", r)
		}
	}
	// The trial-opening half of the flywheel iterates ProductionVersions and asks AdmittedCandidates for each
	// production id; with no production version for the skill it never asks, so the restored row (parent: the
	// RETIRED prior) is reached by no trial.
	prods, err := m.ProductionVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range prods {
		if p.SkillName == "triage-protocol" {
			t.Fatalf("the flywheel must find no production version to open a trial under, got %+v", p)
		}
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
