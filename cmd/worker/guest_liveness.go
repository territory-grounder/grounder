package main

// The guest-liveness feed (TG-378 prereq): after each estate refresh, project the pve source's
// guest power states — read in the SAME /cluster/resources fetch as the placement edges — into the
// durable guest_liveness table, so the start-guest precondition (TG-378 slice 2) has a queryable,
// staleness-honest answer to "is this guest running?". Feeding is measurement, never gating: a write
// error is logged with its denominator and the refresh continues.

import (
	"context"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/modules/cmdb/pve"
)

// guestStateSource is the narrow read this feed needs (satisfied by *pve.EstateSource). ok=false means
// no sweep has completed — nothing is written, so the projection's absence stays honest (unknown, never
// an invented empty cluster).
type guestStateSource interface {
	GuestStates() ([]pve.GuestState, bool)
}

// guestLivenessSink is the narrow write half (satisfied by *db.GuestLivenessStore).
type guestLivenessSink interface {
	Upsert(ctx context.Context, states []db.GuestLivenessState) error
}

// guestLivenessStaleAfter is the FLOOR of the freshness bound the seal-time precondition reads under
// (TG-378): 3× the 5-minute default estate refresh, so one missed sweep never flips a reading to unknown
// but a DEAD sweep does within minutes. The effective bound is livenessBoundFor(configured interval) —
// the sweep cadence is operator-configurable (TG_ESTATE_REFRESH_INTERVAL), and a bound smaller than the
// cadence would make every reading stale before the next sweep lands: fail-closed but structurally DEAD,
// with nothing saying so (the slice-2 reviewer's finding). NEVER pass a non-positive bound in production —
// zero disables the age check (a tests-only escape hatch), and a stale "stopped" trusted past the sweep's
// death is exactly the pve03 guess this gate exists to refuse.
const guestLivenessStaleAfter = 15 * time.Minute

// livenessBoundFor derives the effective freshness bound from the CONFIGURED sweep interval:
// max(15m floor, 3× interval). A non-positive interval (refresh disabled/unset) keeps the floor — the
// projection then ages out after boot and state-preconditioned classes refuse, which the wiring logs
// loudly instead of leaving as a silent structural refusal.
func livenessBoundFor(refreshInterval time.Duration) time.Duration {
	if b := 3 * refreshInterval; b > guestLivenessStaleAfter {
		return b
	}
	return guestLivenessStaleAfter
}

// guestRunningReader adapts the guest_liveness projection to the prediction gate's state seam
// (TG-378). A nil store (no DB) returns nil — the gate treats an unwired seam as UNESTABLISHABLE and
// refuses state-preconditioned classes, which is the correct posture for a projection-less worker.
func guestRunningReader(store *db.GuestLivenessStore, staleAfter time.Duration) func(ctx context.Context, guest string) (bool, string, bool) {
	if store == nil {
		return nil
	}
	return func(ctx context.Context, guest string) (bool, string, bool) {
		running, ok, err := store.Running(ctx, guest, staleAfter)
		if err != nil {
			return false, "guest_liveness read error: " + err.Error(), false
		}
		if !ok {
			return false, "guest_liveness: no fresh observation within " + staleAfter.String(), false
		}
		return running, "guest_liveness, freshness bound " + staleAfter.String(), true
	}
}

// feedGuestLiveness projects the source's last sweep into the sink. Returns the number of states written
// and whether a sweep was available at all — the caller logs both, because "0 of 0 (no sweep yet)" and
// "0 of 0 (cluster listed no guests)" and "wrote 41" are three different facts (TG-365: publish the
// denominator beside the verdict, including at zero).
func feedGuestLiveness(ctx context.Context, sink guestLivenessSink, src guestStateSource) (n int, swept bool, err error) {
	if sink == nil || src == nil {
		return 0, false, nil
	}
	states, ok := src.GuestStates()
	if !ok {
		return 0, false, nil
	}
	rows := make([]db.GuestLivenessState, 0, len(states))
	for _, st := range states {
		// ObservedAt rides along so the monotone Upsert guard can order this 5-min sweep against the ~37s
		// pve-liveness detector's writes (TG-496) — the slow sweep must never clobber the fast feed's
		// fresher STOPPED on a later write.
		rows = append(rows, db.GuestLivenessState{Guest: st.Guest, Node: st.Node, Status: st.Status, ObservedAt: st.ObservedAt})
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := sink.Upsert(cctx, rows); err != nil {
		return 0, true, err
	}
	return len(rows), true, nil
}
