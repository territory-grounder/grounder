package main

// TG-496 fix (freshness): the pve-liveness detector must refresh the guest_liveness projection from its OWN
// ~37s fetch, and that write must land BEFORE the down-envelope is dispatched — otherwise the triage the
// envelope starts reads the up-to-5-min-stale estate sweep, sees the guest still "running", and the
// deterministic guest-down heal + the TG-378 seal gate both refuse (the exact live-drill gap).
//
// KILLING MUTATIONS (execute, watch RED, restore):
//   - move `feedGuestLivenessDetector(...)` in runLivenessPoll to AFTER the mint loop →
//     TestLivenessPollProjectsStoppedBeforeDispatch fails: the down-envelope is dispatched onto a projection
//     that still shows the guest running, the exact regression this fix closes.
//   - drop runLivenessPoll's `if err != nil { return }` fetch guard (project before the fetch-error check) →
//     TestLivenessPollFetchFailureProjectsNothing fails: a failed fetch refreshes stale/last-good state, the
//     honesty invariant ("a failed fetch writes NOTHING") broken.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
	pveliveness "github.com/territory-grounder/grounder/modules/ingest/pveliveness"
)

// fakeLivenessFetcher satisfies livenessFetcher: FetchActive returns the configured envelopes (or fetchErr)
// and GuestStates returns the cached states. Both are what *pveliveness.Source exposes in production.
type fakeLivenessFetcher struct {
	envs     []coreingest.IncidentEnvelope
	states   []pveliveness.GuestState
	ok       bool
	fetchErr error
}

func (f *fakeLivenessFetcher) FetchActive(context.Context) ([]coreingest.IncidentEnvelope, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return f.envs, nil
}
func (f *fakeLivenessFetcher) GuestStates() ([]pveliveness.GuestState, bool) { return f.states, f.ok }

// orderRecordingSink appends one event per upserted guest to a SHARED log, so an ordering test can assert
// the projection write precedes the dispatch of the same cycle's envelopes.
type orderRecordingSink struct {
	events *[]string
	fail   error
}

func (s *orderRecordingSink) Upsert(_ context.Context, states []db.GuestLivenessState) error {
	for _, st := range states {
		*s.events = append(*s.events, "upsert:"+st.Guest+"="+st.Status)
	}
	return s.fail
}

// TestLivenessPollProjectsStoppedBeforeDispatch is the load-bearing ordering oracle: on a running→stopped
// transition, the STOPPED projection must commit BEFORE the down-envelope is dispatched.
func TestLivenessPollProjectsStoppedBeforeDispatch(t *testing.T) {
	var events []string
	sink := &orderRecordingSink{events: &events}
	src := &fakeLivenessFetcher{
		envs:   []coreingest.IncidentEnvelope{{ExternalRef: "tg-liveness-g1-1", Host: "g1"}},
		states: []pveliveness.GuestState{{Guest: "g1", Node: "pve01", Status: "stopped", ObservedAt: time.Now()}},
		ok:     true,
	}
	mint := func(_ context.Context, env coreingest.IncidentEnvelope) {
		events = append(events, "dispatch:"+env.Host)
	}
	res, err := runLivenessPoll(context.Background(), src, sink, mint)
	if err != nil {
		t.Fatalf("unexpected fetch error: %v", err)
	}
	if res.Fetched != 1 || res.Projected != 1 || res.ProjErr != nil {
		t.Fatalf("res = %+v, want Fetched=1 Projected=1 ProjErr=nil", res)
	}
	// The projection of STOPPED commits before the down-envelope is dispatched, so the triage it starts
	// reads a fresh stopped guest_liveness at correlate + seal time (TG-496).
	if len(events) != 2 || events[0] != "upsert:g1=stopped" || events[1] != "dispatch:g1" {
		t.Fatalf("upsert-before-dispatch ordering violated: %v (killing mutation: move the projection after the mint loop)", events)
	}
}

// TestLivenessPollFetchFailureProjectsNothing is the fail-closed/honesty oracle: a failed fetch must project
// nothing and dispatch nothing, even though the (stale) cache still reports ok=true.
func TestLivenessPollFetchFailureProjectsNothing(t *testing.T) {
	var events []string
	sink := &orderRecordingSink{events: &events}
	boom := errors.New("cluster/resources: status 500")
	src := &fakeLivenessFetcher{
		fetchErr: boom,
		// A failed fetch must NOT reach these — writing them would refresh stale state to now() and make a
		// dead sweep look fresh (the honesty invariant this guards).
		states: []pveliveness.GuestState{{Guest: "g1", Status: "stopped", ObservedAt: time.Now()}},
		ok:     true,
	}
	dispatched := 0
	res, err := runLivenessPoll(context.Background(), src, sink, func(context.Context, coreingest.IncidentEnvelope) { dispatched++ })
	if !errors.Is(err, boom) {
		t.Fatalf("a failed fetch must return its error, got %v", err)
	}
	if res.Fetched != 0 || res.Projected != 0 || res.ProjErr != nil {
		t.Fatalf("a failed fetch must project + dispatch nothing: res=%+v", res)
	}
	if dispatched != 0 {
		t.Fatalf("a failed fetch must dispatch nothing, dispatched=%d", dispatched)
	}
	if len(events) != 0 {
		t.Fatalf("a FAILED fetch must upsert NOTHING (honesty: never invent/refresh state), sink saw %v "+
			"(killing mutation: project before the fetch-error return)", events)
	}
}

// TestFeedGuestLivenessDetectorHonesty: before the first successful fetch the detector reports ok=false, so
// the feed writes NOTHING (unknown, never an invented empty cluster); nil seams no-op.
func TestFeedGuestLivenessDetectorHonesty(t *testing.T) {
	var events []string
	sink := &orderRecordingSink{events: &events}
	n, err := feedGuestLivenessDetector(context.Background(), sink, &fakeLivenessFetcher{ok: false})
	if err != nil || n != 0 {
		t.Fatalf("ok=false must write nothing: n=%d err=%v", n, err)
	}
	if len(events) != 0 {
		t.Fatalf("ok=false must upsert NOTHING, sink saw %v", events)
	}
	if n, err := feedGuestLivenessDetector(context.Background(), nil, nil); n != 0 || err != nil {
		t.Fatalf("nil seams must no-op: n=%d err=%v", n, err)
	}
	if n, err := feedGuestLivenessDetector(context.Background(), sink, nil); n != 0 || err != nil {
		t.Fatalf("nil source must no-op: n=%d err=%v", n, err)
	}
}

// TestLivenessPollProjectionErrorStillDispatches: the projection is measurement, never a gate — an Upsert
// error is surfaced for the caller to log, but detection (dispatch) still proceeds.
func TestLivenessPollProjectionErrorStillDispatches(t *testing.T) {
	var events []string
	sink := &orderRecordingSink{events: &events, fail: errors.New("upsert boom")}
	src := &fakeLivenessFetcher{
		envs:   []coreingest.IncidentEnvelope{{ExternalRef: "tg-liveness-g1-1", Host: "g1"}},
		states: []pveliveness.GuestState{{Guest: "g1", Status: "stopped", ObservedAt: time.Now()}},
		ok:     true,
	}
	dispatched := 0
	res, err := runLivenessPoll(context.Background(), src, sink, func(context.Context, coreingest.IncidentEnvelope) { dispatched++ })
	if err != nil {
		t.Fatalf("a projection error is non-fatal to the fetch: %v", err)
	}
	if res.ProjErr == nil {
		t.Fatal("the projection upsert error must be surfaced in ProjErr for the caller to log with its denominator")
	}
	if dispatched != 1 {
		t.Fatalf("the projection is measurement, never a gate — dispatch must still run, dispatched=%d", dispatched)
	}
}

// TestFeedGuestLivenessDetectorNilStoreNoPanic is the TG-496 regression for the typed-nil sink crash. A
// no-DSN worker's guestLivenessStore.Load() is a nil *db.GuestLivenessStore; boxed into the sink INTERFACE
// it is a TYPED-NIL that slips past feedGuestLivenessDetector's `sink == nil` guard. Without the
// nil-receiver guard on GuestLivenessStore.Upsert, the first non-empty feed dereferences the nil receiver
// and panics — crash-looping the worker in a configuration meant to degrade honestly. Passing a REAL nil
// *db.GuestLivenessStore (not the fake) must NO-OP without panicking.
//
// KILLING MUTATION (execute, watch RED, restore): remove the `if s == nil { return nil }` guard at the top
// of GuestLivenessStore.Upsert → this test PANICS (nil-pointer dereference on s.p), the exact crash.
func TestFeedGuestLivenessDetectorNilStoreNoPanic(t *testing.T) {
	var nilStore *db.GuestLivenessStore // nil concrete pointer → a typed-nil once boxed into guestLivenessSink
	src := &fakeLivenessFetcher{
		states: []pveliveness.GuestState{{Guest: "g1", Status: "stopped", ObservedAt: time.Now()}},
		ok:     true,
	}
	// Boxing nilStore into the sink interface param is exactly what the pre-fix call site did.
	if _, err := feedGuestLivenessDetector(context.Background(), nilStore, src); err != nil {
		t.Fatalf("a nil store must no-op without error (the honest no-pool degrade), got %v", err)
	}
	// Reaching here without a panic IS the assertion: a nil-receiver Upsert would have panicked the test.
}
