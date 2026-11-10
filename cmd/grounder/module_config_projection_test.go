package main

// ORACLES FOR THE CAPABILITY-PROJECTION MERGE (TG-251). Each names its killing mutation.
//
// The defect these keep dead: the modules page asserted 28 worker-resident connectors were switched off
// because the API process's registry could not see them. The projection is the channel; the staleness
// cutoff is what keeps the channel honest when its publisher dies.

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
)

func schemaFor(t *testing.T, rows []db.CapabilityProjectionRow, at time.Time) map[string][2]bool {
	t.Helper()
	c := catalogSchema{fleet: fleetView{
		projection: func(context.Context) ([]db.CapabilityProjectionRow, error) { return rows, nil },
		staleAfter: 3 * time.Minute,
		now:        func() time.Time { return at },
	}}
	page, err := c.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][2]bool{}
	for _, m := range page.Modules {
		out[m.Surface+"/"+m.SourceType] = [2]bool{m.Enabled, m.EnabledKnown}
	}
	return out
}

// KILLING MUTATION: drop the projection merge (revert to registry-only). RED — the exact TG-251 defect:
// a worker-resident module the worker vouches for still renders "state not reported here".
func TestAFreshProjectionRowAnswersForAWorkerResidentModule(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	got := schemaFor(t, []db.CapabilityProjectionRow{
		{Surface: "notifier", SourceType: "matrix", Enabled: true, ObservedAt: now.Add(-30 * time.Second)},
	}, now)
	st, ok := got["notifier/matrix"]
	if !ok {
		t.Fatal("notifier/matrix absent from the schema page")
	}
	if !st[1] || !st[0] {
		t.Fatalf("fresh worker-published row: enabled=%v enabled_known=%v, want true/true — the projection "+
			"channel is not being read", st[0], st[1])
	}
}

// THE TICKET'S OWN ACCEPTANCE, AS AN ORACLE RATHER THAN INSPECTION. KILLING MUTATION: remove the
// observed_at cutoff (serve any row forever). RED — a dead worker's last words would keep answering.
func TestAStaleProjectionRowDegradesToUnknown(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	got := schemaFor(t, []db.CapabilityProjectionRow{
		{Surface: "notifier", SourceType: "matrix", Enabled: true, ObservedAt: now.Add(-4 * time.Minute)},
	}, now)
	st := got["notifier/matrix"]
	if st[1] {
		t.Fatal("a row 4m old (window 3m) still reports enabled_known=true — the projection outlived its " +
			"publisher, which is the original defect one layer down")
	}
	if st[0] {
		t.Fatal("a stale row still reports enabled=true")
	}
}

// KILLING MUTATION: let a projected row overwrite the local registry's answer. RED — this process's own
// registry is ground truth for what runs IN it; the projection answers only for modules that live elsewhere.
// (Driven with a disabled local pair vs an enabled projected row for the same pair: local must win. The
// local registry here is nil, so the control is the disabled-row direction instead: a fresh projected row
// with Enabled=false must yield enabled_known=true, enabled=false — known-off, not unknown.)
func TestAFreshDisabledRowIsKnownOffNotUnknown(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	got := schemaFor(t, []db.CapabilityProjectionRow{
		{Surface: "tracker", SourceType: "youtrack", Enabled: false, ObservedAt: now.Add(-10 * time.Second)},
	}, now)
	st := got["tracker/youtrack"]
	if !st[1] {
		t.Fatal("a fresh Enabled=false row reads as unknown — 'the worker looked and it is off' and 'nobody " +
			"can see it' must stay distinct states")
	}
	if st[0] {
		t.Fatal("a fresh Enabled=false row reads as enabled")
	}
}

// KILLING MUTATION: propagate a projection read error as a page error. RED — a config-plane blip must
// degrade the enablement column to unknown, not take down the whole schema surface (forms, undescribed
// list) with it.
func TestAProjectionReadErrorDegradesNotErrors(t *testing.T) {
	c := catalogSchema{fleet: fleetView{
		projection: func(context.Context) ([]db.CapabilityProjectionRow, error) {
			return nil, context.DeadlineExceeded
		},
	}}
	page, err := c.Schema(context.Background())
	if err != nil {
		t.Fatalf("a projection read failure errored the schema page: %v", err)
	}
	if len(page.Modules) == 0 {
		t.Fatal("schema page empty — the degrade dropped real content")
	}
	for _, m := range page.Modules {
		if m.EnabledKnown {
			t.Fatalf("%s/%s claims a known state with no registry and a failed projection read",
				m.Surface, m.SourceType)
		}
	}
}
