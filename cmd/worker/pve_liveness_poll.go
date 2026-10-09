package main

// pve_liveness_poll.go — ONE pve-liveness poll cycle, with the load-bearing TG-496 ordering (project the
// fresh watched-guest states into guest_liveness BEFORE dispatching the down-envelopes) factored out of
// main()'s ticker goroutine so it is unit-testable.
//
// WHY THE ORDERING IS LOAD-BEARING. A confirmed guest-down runs a DETERMINISTIC start-guest heal
// (temporal/runner/deterministic_heal.go, confirmedGuestDownHeal) only when the guest reads OBSERVED-STOPPED
// in the guest_liveness projection — the same projection the TG-378 seal gate re-reads at commit. That
// projection used to be fed ONLY by the 5-min estate sweep, so at correlate time (~0.4s after a ~37s
// detection) it still showed the guest "running" (up to 5 min stale) and the fast-path never engaged — a
// live drill proved it unreachable. This cycle refreshes the projection from the detector's OWN ~37s fetch,
// and commits that write BEFORE the envelope is dispatched to Temporal, so the triage the envelope starts
// reads the fresh STOPPED state instead of the stale sweep.

import (
	"context"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
	pveliveness "github.com/territory-grounder/grounder/modules/ingest/pveliveness"
)

// guestStateDetector is the detector's cached-state read: the watched guests' running/stopped states from
// its last successful fetch, plus whether any fetch has succeeded. Satisfied by *pveliveness.Source. It is
// deliberately DISTINCT from guestStateSource (the estate sweep's, over pve.GuestState): the detector speaks
// its own module-local GuestState so the ingest package needs no cmdb dependency — the field shapes match.
type guestStateDetector interface {
	GuestStates() ([]pveliveness.GuestState, bool)
}

// livenessFetcher is the detector's poll surface: fetch the active running→stopped transitions AND expose
// the states cached from that SAME fetch. Satisfied by *pveliveness.Source.
type livenessFetcher interface {
	FetchActive(ctx context.Context) ([]coreingest.IncidentEnvelope, error)
	guestStateDetector
}

// feedGuestLivenessDetector projects the detector's cached watched-guest states into the guest_liveness
// sink. It is the fast-feed sibling of feedGuestLiveness (the 5-min estate sweep's feed): same table,
// latest-OBSERVED-wins, but it reads the detector's module-local GuestState so the ingest package needs no
// cmdb dependency. HONESTY: ok=false (no successful fetch yet) or a nil sink/src writes NOTHING — the
// projection stays unknown, never an invented empty cluster. Returns the count written (the denominator for
// the caller's log) and the upsert error (NON-FATAL: the caller logs it; the projection is measurement,
// never a gate on detection).
func feedGuestLivenessDetector(ctx context.Context, sink guestLivenessSink, src guestStateDetector) (int, error) {
	if sink == nil || src == nil {
		return 0, nil
	}
	states, ok := src.GuestStates()
	if !ok {
		return 0, nil // no successful fetch yet ⇒ write nothing (honesty: unknown, not empty)
	}
	rows := make([]db.GuestLivenessState, 0, len(states))
	for _, st := range states {
		rows = append(rows, db.GuestLivenessState{Guest: st.Guest, Node: st.Node, Status: st.Status, ObservedAt: st.ObservedAt})
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := sink.Upsert(cctx, rows); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// livenessPollResult is one cycle's outcome for the caller's registers and logs.
type livenessPollResult struct {
	Fetched   int   // down-envelopes dispatched this cycle (== observed running→stopped transitions)
	Projected int   // watched-guest states upserted into guest_liveness (the projection denominator)
	ProjErr   error // projection upsert error — NON-FATAL: the caller logs it; dispatch still ran
}

// runLivenessPoll executes ONE poll cycle with the ordering the TG-496 deterministic guest-down fast-path
// depends on: FetchActive → PROJECT the fresh watched-guest states into guest_liveness → THEN dispatch each
// down-envelope via mint. The projection MUST commit before dispatch so the triage each envelope starts
// reads a fresh (stopped) projection rather than the up-to-5-min-stale estate sweep.
//
// Fail-closed / honesty:
//   - a FetchActive error projects NOTHING and dispatches NOTHING (returns it; the caller records the
//     failure, logs, and retries next tick) — a failed fetch never writes invented/stale state;
//   - the projection is MEASUREMENT, not a gate: an Upsert error is returned in ProjErr for the caller to
//     log with its denominator, but dispatch STILL proceeds (a triage without the fast path still heals via
//     the normal loop) — the projection never blocks detection;
//   - before the detector's first successful fetch GuestStates() is ok=false, so nothing is written.
func runLivenessPoll(ctx context.Context, src livenessFetcher, sink guestLivenessSink, mint func(context.Context, coreingest.IncidentEnvelope)) (livenessPollResult, error) {
	envs, err := src.FetchActive(ctx)
	if err != nil {
		return livenessPollResult{}, err
	}
	// Project BEFORE dispatch — this ordering is the whole point of the file (see the package note above).
	n, projErr := feedGuestLivenessDetector(ctx, sink, src)
	for _, env := range envs {
		mint(ctx, env)
	}
	return livenessPollResult{Fetched: len(envs), Projected: n, ProjErr: projErr}, nil
}
