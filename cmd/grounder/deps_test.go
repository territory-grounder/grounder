package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/safety"
)

// fakePosture is the in-memory twin of *db.PostureReadStore for the oracle tests (CI has no Postgres).
type fakePosture struct {
	row db.PostureRow
	err error
}

func (f fakePosture) Latest(_ context.Context, _ string) (db.PostureRow, error) { return f.row, f.err }

// TestBuildPublicAPIMountsReadOnlySurface is the regression guard for the composition gap this change
// fixes: the server binary built its router inline and never called httpapi.Register, so /v1/stats and
// session-replay — though built, contracted, and unit-tested — were NOT served. This walks the exact router
// main serves and asserts the whole read-only surface is mounted. A zero-value Verifier is fine: walking
// registered routes never authenticates (same pattern as gencontracts.BuildModel).
func TestBuildPublicAPIMountsReadOnlySurface(t *testing.T) {
	api := buildPublicAPI(&auth.Verifier{}, safety.NewReadOnlyChokepoint(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, 0, nil)

	routes, ok := api.Mux().(chi.Routes)
	if !ok {
		t.Fatal("router does not expose chi.Routes")
	}
	got := map[string]bool{}
	if err := chi.Walk(routes, func(_ string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		got[route] = true
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}

	for _, want := range []string{
		"/v1/whoami",
		"/v1/stats",
		"/v1/sessions/{external_ref}/replay",
		"/v1/ledger",
		"/v1/ingest/{source_type}",
		"/v1/capabilities",
		"/v1/wiki",
		"/v1/wiki/{slug}",
		"/v1/credentials/sources",
		"/v1/credentials/resolutions",
		"/v1/credentials/coverage",
	} {
		if !got[want] {
			t.Errorf("public API does not mount %q — the read-only surface is not served (got routes: %v)", want, got)
		}
	}
}

// TestGateStatsFallsBackToUnknownWithoutWorkerPosture proves the grounder never reports a confident false
// OFF when it has no fresh worker posture: with no posture reader it falls back to its own read-only gate
// (false) but FLAGS the reading stale/unknown (source=grounder-gate), so the console shows "unknown", not a
// vouched-for OFF. Counters stay honest zeros.
func TestGateStatsFallsBackToUnknownWithoutWorkerPosture(t *testing.T) {
	gate := safety.NewReadOnlyChokepoint()
	s, err := gateStats{gate: gate}.Stats(context.Background(), auth.Principal{})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if s.MutationEnabled {
		t.Error("no worker posture must fall back to the read-only gate (false), not invent ON")
	}
	if !s.PostureStale {
		t.Error("no worker posture must be flagged stale/unknown, not a confident OFF")
	}
	if s.PostureSource != "grounder-gate" {
		t.Errorf("posture_source = %q, want grounder-gate", s.PostureSource)
	}
	if s.OpenSessions != 0 || s.PendingPolls != 0 {
		t.Errorf("unwired counters must report zero, not a fabricated number: got %d/%d", s.OpenSessions, s.PendingPolls)
	}
}

// TestGateStatsReportsWorkerPosture proves the fix: the grounder reports the WORKER's published live posture,
// not its own gate. A worker with mutation ON is reported ON even though the grounder's own gate is (and must
// stay) OFF — closing the safety-honesty gap where the console showed "read-only" while the worker could act.
func TestGateStatsReportsWorkerPosture(t *testing.T) {
	gate := safety.NewReadOnlyChokepoint() // grounder gate stays OFF (INV-09)
	fresh := db.PostureRow{Found: true, MutationEnabled: true, EffectCapability: "ssh", UpdatedAt: time.Now()}
	s, err := gateStats{gate: gate, posture: fakePosture{row: fresh}, staleAfter: time.Minute}.Stats(context.Background(), auth.Principal{})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !s.MutationEnabled {
		t.Error("a live-ON worker must be reported ON, even though the grounder gate is OFF")
	}
	if s.PostureStale {
		t.Error("a fresh worker row must not be flagged stale")
	}
	if s.PostureSource != "worker" {
		t.Errorf("posture_source = %q, want worker", s.PostureSource)
	}
	if gate.MayActuate() {
		t.Error("reporting must NOT touch the grounder's own gate — it must remain OFF (INV-09)")
	}
}

// TestGateStatsStaleWorkerPostureIsUnknown proves a STALE worker row is surfaced as its freshest reading BUT
// flagged unknown — never silently downgraded to a confident OFF, so an ON worker can never hide behind a
// heartbeat gap.
func TestGateStatsStaleWorkerPostureIsUnknown(t *testing.T) {
	gate := safety.NewReadOnlyChokepoint()
	stale := db.PostureRow{Found: true, MutationEnabled: true, UpdatedAt: time.Now().Add(-10 * time.Minute)}
	s, err := gateStats{gate: gate, posture: fakePosture{row: stale}, staleAfter: time.Minute}.Stats(context.Background(), auth.Principal{})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !s.MutationEnabled {
		t.Error("a stale-but-ON worker row must still surface ON (the freshest reading), never a false OFF")
	}
	if !s.PostureStale {
		t.Error("a stale worker row must be flagged stale/unknown")
	}
	if s.PostureSource != "worker-stale" {
		t.Errorf("posture_source = %q, want worker-stale", s.PostureSource)
	}
}

// TestResolvePosture exercises the shared resolver directly across all branches, including the
// effect-capability propagation and the fail-safe fallback on a read error.
func TestResolvePosture(t *testing.T) {
	gate := safety.NewReadOnlyChokepoint()
	if v := resolvePosture(context.Background(), nil, gate, time.Minute); v.MutationEnabled || !v.Stale || v.Source != "grounder-gate" {
		t.Errorf("nil reader: got %+v, want off/stale/grounder-gate", v)
	}
	fresh := fakePosture{row: db.PostureRow{Found: true, MutationEnabled: true, EffectCapability: "actuation.local.readonly", UpdatedAt: time.Now()}}
	if v := resolvePosture(context.Background(), fresh, gate, time.Minute); !v.MutationEnabled || v.Stale || v.Source != "worker" || v.EffectCapability != "actuation.local.readonly" {
		t.Errorf("fresh worker: got %+v", v)
	}
	if v := resolvePosture(context.Background(), fakePosture{err: errors.New("boom")}, gate, time.Minute); v.MutationEnabled || !v.Stale || v.Source != "grounder-gate" {
		t.Errorf("read error must fall back to the fail-safe unknown: got %+v", v)
	}
}

// TestNoSnapshotsResolvesToNotFound proves the default snapshot store fails closed to the 404 path (found=
// false) rather than nil-dereferencing or inventing a snapshot — an unknown and an unauthorized id are
// observationally identical (REQ-504).
func TestNoSnapshotsResolvesToNotFound(t *testing.T) {
	_, found, err := noSnapshots{}.Get(context.Background(), "anything", auth.Principal{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Error("no-op snapshot store must never report a snapshot found")
	}
}

// TestNoStarterNeverSilentlySucceeds proves the no-op starter returns a non-nil error, so a future wiring
// mistake that reached it (bypassing the found=false short-circuit) fails closed instead of minting a bogus
// workflow id.
func TestNoStarterNeverSilentlySucceeds(t *testing.T) {
	id, err := noStarter{}.StartFromSnapshot(context.Background(), httpapi.ContextSnapshot{})
	if err == nil {
		t.Error("no-op starter must fail closed, not return a workflow id")
	}
	if id != "" {
		t.Errorf("no-op starter must not return a workflow id, got %q", id)
	}
}
