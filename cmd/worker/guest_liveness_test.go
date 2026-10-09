package main

// TG-378: the glue between the pve source's cached sweep and the guest_liveness projection. The wiring
// half matters as much as the store — a projection nothing feeds reads permanently unknown, which is
// fail-closed but useless (the "present, not reaching" shape).
//
// KILLING MUTATION (executed 2026-08-11): make feedGuestLiveness treat ok=false as an empty sweep (write
// anyway) — TestFeedWithoutASweepWritesNothing fails, because "never read the cluster" projected as an
// empty write would AGE every existing row toward unknown no faster, but a later mutation writing zero
// rows as truth would; the distinction swept=false must reach the caller. Restore → green.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/modules/cmdb/pve"
)

type fakeGuestStates struct {
	states []pve.GuestState
	ok     bool
}

func (f fakeGuestStates) GuestStates() ([]pve.GuestState, bool) { return f.states, f.ok }

type fakeLivenessSink struct {
	got  [][]db.GuestLivenessState
	fail error
}

func (f *fakeLivenessSink) Upsert(_ context.Context, states []db.GuestLivenessState) error {
	f.got = append(f.got, states)
	return f.fail
}

func TestFeedProjectsTheSweep(t *testing.T) {
	sink := &fakeLivenessSink{}
	src := fakeGuestStates{states: []pve.GuestState{
		{Guest: "g1", Node: "pve01", Status: "running"},
		{Guest: "g2", Node: "pve02", Status: "stopped"},
	}, ok: true}
	n, swept, err := feedGuestLiveness(context.Background(), sink, src)
	if err != nil || !swept || n != 2 {
		t.Fatalf("feed: n=%d swept=%v err=%v", n, swept, err)
	}
	if len(sink.got) != 1 || len(sink.got[0]) != 2 || sink.got[0][0].Guest != "g1" || sink.got[0][1].Status != "stopped" {
		t.Fatalf("sink saw %+v", sink.got)
	}
}

func TestFeedWithoutASweepWritesNothing(t *testing.T) {
	sink := &fakeLivenessSink{}
	n, swept, err := feedGuestLiveness(context.Background(), sink, fakeGuestStates{ok: false})
	if err != nil || swept || n != 0 {
		t.Fatalf("no sweep must be (0,false,nil): n=%d swept=%v err=%v", n, swept, err)
	}
	if len(sink.got) != 0 {
		t.Fatalf("a source with no completed sweep must write NOTHING, sink saw %+v", sink.got)
	}
}

func TestFeedNilSeamsAreQuietNoOps(t *testing.T) {
	if n, swept, err := feedGuestLiveness(context.Background(), nil, nil); n != 0 || swept || err != nil {
		t.Fatalf("nil seams must no-op: n=%d swept=%v err=%v", n, swept, err)
	}
	if n, swept, err := feedGuestLiveness(context.Background(), &fakeLivenessSink{}, nil); n != 0 || swept || err != nil {
		t.Fatalf("nil source must no-op: n=%d swept=%v err=%v", n, swept, err)
	}
}

func TestFeedSurfacesTheSinkError(t *testing.T) {
	boom := errors.New("boom")
	sink := &fakeLivenessSink{fail: boom}
	_, swept, err := feedGuestLiveness(context.Background(), sink, fakeGuestStates{states: []pve.GuestState{{Guest: "g"}}, ok: true})
	if !swept || !errors.Is(err, boom) {
		t.Fatalf("a sink error must surface with swept=true (the caller logs it): swept=%v err=%v", swept, err)
	}
}

// TestLivenessBoundTracksTheConfiguredCadence (the !1316 reviewer's Medium): the freshness bound must
// never be smaller than the sweep cadence can satisfy — max(15m floor, 3× interval).
// KILLING MUTATION (executed 2026-08-11): make livenessBoundFor return the floor unconditionally — the
// 10m case fails (30m expected), because a 20-30m operator cadence would make every reading stale before
// the next sweep and start-guest would refuse forever with nothing saying so.
func TestLivenessBoundTracksTheConfiguredCadence(t *testing.T) {
	for _, tc := range []struct {
		interval, want time.Duration
	}{
		{0, guestLivenessStaleAfter},               // refresh disabled → floor (and the wiring logs loudly)
		{2 * time.Minute, guestLivenessStaleAfter}, // fast sweep → floor still governs
		{10 * time.Minute, 30 * time.Minute},       // slow sweep → 3× cadence outgrows the floor
	} {
		if got := livenessBoundFor(tc.interval); got != tc.want {
			t.Fatalf("livenessBoundFor(%s) = %s, want %s", tc.interval, got, tc.want)
		}
	}
}
