package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/schedule"
	"github.com/territory-grounder/grounder/modules/schedule/cronicle"
)

// TG-411 arms the spec/019 maintenance-window sensor: the worker composition root now constructs the
// Cronicle connector behind TG_CRONICLE_DEPLOYMENTS and projects an ACTIVE maintenance window into the
// tier-1 suppression freeze plane. These oracles pin the three properties that make that safe:
//   (1) an active maintenance window becomes a freeze window covering now (the capability is real, not a
//       logged-but-inert count) — killing mutation: drop the append in maintenanceFreezeWindows → RED;
//   (2) an INACTIVE window and a change-FREEZE window contribute nothing (narrow by construction);
//   (3) unset config yields no providers and an unreadable scheduler yields no freeze (inert / fail-closed).

// noon is a fixed instant so ActiveSpan is deterministic (no wall-clock dependence).
var noon = time.Date(2026, 8, 8, 12, 0, 30, 0, time.UTC)

// maintAt builds a maintenance WindowRule whose single daily occurrence starts at hh:mm and runs for dur.
func maintAt(kind schedule.WindowKind, target, title string, hh, mm int, dur time.Duration) schedule.WindowRule {
	return schedule.WindowRule{
		Kind:     kind,
		Target:   target,
		Title:    title,
		Rec:      schedule.Recurrence{Hours: []int{hh}, Minutes: []int{mm}},
		Duration: dur,
		Loc:      time.UTC,
	}
}

func TestMaintenanceFreezeWindowsProjectsActiveWindow(t *testing.T) {
	cal := schedule.Calendar{Readable: true, Windows: []schedule.WindowRule{
		maintAt(schedule.KindMaintenance, "dc1gitea01", "gitea kernel patch", 12, 0, time.Hour),
	}}
	got := maintenanceFreezeWindows(cal, noon)
	if len(got) != 1 {
		t.Fatalf("an active maintenance window must project exactly one freeze window, got %d — the sensor is "+
			"wired but yields nothing", len(got))
	}
	w := got[0]
	if w.Scope != "dc1gitea01" {
		t.Errorf("scope = %q, want the window's host so only in-scope alerts freeze", w.Scope)
	}
	if !(!noon.Before(w.Start) && noon.Before(w.End)) {
		t.Errorf("now %s must fall inside the projected span [%s, %s)", noon, w.Start, w.End)
	}
	// The span must be the concrete occurrence [12:00, 13:00), not an open-ended freeze.
	if want := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC); !w.Start.Equal(want) {
		t.Errorf("start = %s, want the occurrence start %s", w.Start, want)
	}
	if want := time.Date(2026, 8, 8, 13, 0, 0, 0, time.UTC); !w.End.Equal(want) {
		t.Errorf("end = %s, want start+duration %s", w.End, want)
	}
}

func TestMaintenanceFreezeWindowsIsNarrow(t *testing.T) {
	cal := schedule.Calendar{Readable: true, Windows: []schedule.WindowRule{
		// a maintenance window that is NOT active at noon (starts 03:00, 1h) — must not freeze.
		maintAt(schedule.KindMaintenance, "h1", "nightly", 3, 0, time.Hour),
		// a change-FREEZE window active at noon — a freeze means "no change is sanctioned", NOT an
		// expected-alert span, so it must not be projected into the suppression freeze plane.
		maintAt(schedule.KindFreeze, "h2", "code freeze", 12, 0, time.Hour),
	}}
	if got := maintenanceFreezeWindows(cal, noon); len(got) != 0 {
		t.Fatalf("neither an inactive maintenance window nor an active change-freeze may suppress, got %d: %+v", len(got), got)
	}
}

func TestMaintenanceFreezeWindowsSkipsGlobScope(t *testing.T) {
	// A glob target ("dc1*") is active in the scheduler but the FreezeGate matches scope EXACTLY, so
	// projecting it would create a window that never equals a concrete alert host — a silent no-op. It must
	// be dropped (visibly, via a log) rather than passed through. Killing mutation: remove the glob-skip →
	// this window projects and len becomes 1 → RED.
	cal := schedule.Calendar{Readable: true, Windows: []schedule.WindowRule{
		maintAt(schedule.KindMaintenance, "dc1*", "rolling dc1 patch", 12, 0, time.Hour),
	}}
	if got := maintenanceFreezeWindows(cal, noon); len(got) != 0 {
		t.Fatalf("a glob-scoped maintenance window must not project a freeze window the gate can never match, got %+v", got)
	}
	// A bare "*" is NOT a glob-narrow — it is the estate-wide idiom and still projects.
	estate := schedule.Calendar{Readable: true, Windows: []schedule.WindowRule{
		maintAt(schedule.KindMaintenance, "*", "estate-wide", 12, 0, time.Hour),
	}}
	if got := maintenanceFreezeWindows(estate, noon); len(got) != 1 || got[0].Scope != "" {
		t.Fatalf("'*' must still project one estate-wide freeze (Scope \"\"), got %+v", got)
	}
}

func TestScheduleScopeWildcardIsEstateWide(t *testing.T) {
	cal := schedule.Calendar{Readable: true, Windows: []schedule.WindowRule{
		maintAt(schedule.KindMaintenance, "*", "estate-wide maintenance", 12, 0, time.Hour),
	}}
	got := maintenanceFreezeWindows(cal, noon)
	if len(got) != 1 || got[0].Scope != "" {
		t.Fatalf("a '*' target must become an estate-wide freeze (Scope \"\"), got %+v", got)
	}
	// unit-level for the mapping itself.
	for in, want := range map[string]string{"*": "", "": "", "dc1db01": "dc1db01"} {
		if s := scheduleScope(in); s != want {
			t.Errorf("scheduleScope(%q) = %q, want %q", in, s, want)
		}
	}
}

func TestCronicleProvidersInertByDefault(t *testing.T) {
	if p := cronicleProviders(""); p != nil {
		t.Fatalf("an unset TG_CRONICLE_DEPLOYMENTS must construct NO providers (sensor dark), got %d", len(p))
	}
	// A well-formed declaration constructs exactly one provider (the key is resolved lazily, so no secret is
	// needed to build it) — this is the composition-root line spec/019 was missing.
	if p := cronicleProviders("demo|http://127.0.0.1:3012|env:TG_TEST_CRONICLE_KEY"); len(p) != 1 {
		t.Fatalf("a valid deployment row must construct one provider, got %d", len(p))
	}
}

// failDoer simulates an unreachable scheduler (transport error), so Snapshot fails and the fail-closed path
// runs. keyRef resolves from the env set below, so the failure is the DOER, not a missing key.
type failDoer struct{}

func (failDoer) Do(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("dial tcp 127.0.0.1:3012: connect: connection refused")
}

func TestCronicleFreezeWindowsFailsClosedOnUnreadable(t *testing.T) {
	t.Setenv("TG_TEST_CRONICLE_KEY", "k")
	c, err := cronicle.New(cronicle.Config{
		BaseURL: "http://127.0.0.1:3012", KeyRef: "env:TG_TEST_CRONICLE_KEY", HTTPClient: failDoer{},
	})
	if err != nil {
		t.Fatalf("client build: %v", err)
	}
	p, err := cronicle.NewProvider(cronicle.ProviderConfig{Client: c, Source: "demo"})
	if err != nil {
		t.Fatalf("provider build: %v", err)
	}
	got := cronicleFreezeWindows(context.Background(), []*cronicle.Provider{p}, noon)
	if len(got) != 0 {
		t.Fatalf("an unreadable scheduler must contribute NO freeze window (fail-closed: estate stays open to "+
			"triage), got %d: %+v", len(got), got)
	}
}
