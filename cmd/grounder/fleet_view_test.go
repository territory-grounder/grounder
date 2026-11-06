package main

// ORACLES FOR THE ONE MERGED FLEET VIEW (TG-268).
//
// The load-bearing test is the LAST: the two surfaces the Modules page renders side by side — the
// capability banner (/v1/capabilities) and the module dialogs (/v1/modules/schema) — must be incapable of
// reporting different fleets. They disagreed live on 2026-08-03 (15 across 4 families vs 29 across 10)
// because each read its own source, and the banner was the one making a safety claim.

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/modules"
)

func fleetFixture(t *testing.T, at time.Time, rows []db.CapabilityProjectionRow) fleetView {
	t.Helper()
	reg := modules.NewRegistry()
	// A LOCAL module: what this process runs itself.
	if err := reg.Register(modules.Registration{
		Surface: "ingest", SourceType: "librenms", Capability: "ingest.librenms",
		Enabled: true, Adapter: struct{}{},
	}); err != nil {
		t.Fatal(err)
	}
	return fleetView{
		reg:        reg,
		projection: func(context.Context) ([]db.CapabilityProjectionRow, error) { return rows, nil },
		staleAfter: 3 * time.Minute,
		now:        func() time.Time { return at },
	}
}

// KILLING MUTATION: revert /v1/capabilities to the bare local registry. RED — the banner reports 1
// capability while the dialogs render 2, which is the live defect at smaller scale.
func TestCapabilitiesReportsTheMergedFleetNotTheLocalRegistry(t *testing.T) {
	at := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	f := fleetFixture(t, at, []db.CapabilityProjectionRow{
		{Surface: "notifier", SourceType: "matrix", Capability: "notifier.matrix", Enabled: true, ObservedAt: at.Add(-30 * time.Second)},
	})
	caps := f.Capabilities()
	if len(caps) != 2 {
		t.Fatalf("capabilities reported %d, want 2 (1 local + 1 worker-published) — a surface that describes "+
			"the fleet must not report only the half this process happens to run", len(caps))
	}
	seen := map[string]bool{}
	for _, c := range caps {
		seen[c.Surface+"/"+c.SourceType] = true
	}
	if !seen["ingest/librenms"] || !seen["notifier/matrix"] {
		t.Fatalf("merged view lost a member: %v", seen)
	}
}

// KILLING MUTATION: drop the staleness cutoff in entries(). RED — a dead worker's last words would keep
// inflating the capability count, which is the TG-251 failure mode reappearing in this surface.
func TestAStaleProjectionRowLeavesTheCapabilityCount(t *testing.T) {
	at := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	f := fleetFixture(t, at, []db.CapabilityProjectionRow{
		{Surface: "notifier", SourceType: "matrix", Enabled: true, ObservedAt: at.Add(-4 * time.Minute)},
	})
	if got := len(f.Capabilities()); got != 1 {
		t.Fatalf("reported %d capabilities, want 1 — a row 4m old (window 3m) is still being counted", got)
	}
}

// KILLING MUTATION: let a projected row overwrite a local one. RED — this process's own registry is
// ground truth for what runs in it.
func TestLocalRegistryWinsOverTheProjection(t *testing.T) {
	at := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	f := fleetFixture(t, at, []db.CapabilityProjectionRow{
		{Surface: "ingest", SourceType: "librenms", Enabled: false, ObservedAt: at.Add(-10 * time.Second)},
	})
	caps := f.Capabilities()
	if len(caps) != 1 {
		t.Fatalf("got %d capabilities, want 1 — the overlapping pair was double-counted", len(caps))
	}
	if !caps[0].Enabled {
		t.Fatal("a projected row overrode the LOCAL registry's enablement — this process knows what runs in it")
	}
}

// KILLING MUTATION: error the surface on a projection read failure. RED — a config-plane blip must
// degrade to the local view, never blank a page whose other content is still true.
func TestAProjectionFailureDegradesToTheLocalView(t *testing.T) {
	reg := modules.NewRegistry()
	_ = reg.Register(modules.Registration{Surface: "ingest", SourceType: "librenms", Enabled: true, Adapter: struct{}{}})
	f := fleetView{reg: reg, projection: func(context.Context) ([]db.CapabilityProjectionRow, error) {
		return nil, context.DeadlineExceeded
	}}
	if got := len(f.Capabilities()); got != 1 {
		t.Fatalf("got %d, want the 1 local capability — a failed projection read must not empty the fleet", got)
	}
}

// THE ONE THAT ENDS THE PATTERN. Both surfaces the Modules page renders side by side derive from the SAME
// view, so they cannot report different fleets.
//
// KILLING MUTATION: give either surface its own source again — the shape of TG-251, TG-267 and TG-268,
// each of which was a consumer nobody re-pointed. RED, naming the divergence.
func TestTheBannerAndTheDialogsCannotReportDifferentFleets(t *testing.T) {
	at := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	f := fleetFixture(t, at, []db.CapabilityProjectionRow{
		{Surface: "notifier", SourceType: "matrix", Enabled: true, ObservedAt: at.Add(-20 * time.Second)},
		{Surface: "tracker", SourceType: "youtrack", Enabled: false, ObservedAt: at.Add(-20 * time.Second)},
	})

	// The banner's source.
	banner := map[string]bool{}
	for _, c := range f.Capabilities() {
		banner[c.Surface+"/"+c.SourceType] = c.Enabled
	}

	// The dialogs' source — the same view, through the schema page's own entry point.
	dialogs := map[string]bool{}
	for k, e := range (catalogSchema{fleet: f}).fleet.entries(context.Background()) {
		if e.known {
			dialogs[k] = e.enabled
		}
	}

	if len(banner) != len(dialogs) {
		t.Fatalf("the capability banner reports %d members and the module dialogs %d — the two halves of one "+
			"page disagree about the fleet, and the banner is the one asserting a safety invariant (TG-268)",
			len(banner), len(dialogs))
	}
	for k, enabled := range banner {
		got, present := dialogs[k]
		if !present {
			t.Fatalf("%s is in the capability banner but not the dialogs", k)
		}
		if got != enabled {
			t.Fatalf("%s: banner says enabled=%v, dialogs say %v", k, enabled, got)
		}
	}
}
