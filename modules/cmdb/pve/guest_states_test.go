package pve

// TG-378: the same /cluster/resources fetch that yields placement edges carries each guest's power state,
// and discarding it left "is guest X running?" unanswerable estate-wide while TG proposed `start` on guests
// with 2,000-hour uptimes. These oracles pin the state side of the fetch.
//
// KILLING MUTATION (executed 2026-08-11): drop the `Status` field from the clusterResources decode struct —
// TestGuestStatesRideTheSameFetch fails with `status "" want "running"`. Restore → green. Second mutation:
// make GuestStates return `nil, true` before any sweep — TestGuestStatesBeforeAnySweepIsUnknown fails,
// because "never read the cluster" must be distinguishable from "cluster listed nothing" (TG-365: absent
// is not empty).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

func TestGuestStatesRideTheSameFetch(t *testing.T) {
	t.Setenv("TG_TEST_PVE_TOKEN", "root@pam!tg=uuid")
	body := `{"data":[
		{"type":"lxc","node":"dc1pve01","name":"n8n01","status":"running"},
		{"type":"qemu","node":"dc1pve04","name":"dc1nc01","status":"running"},
		{"type":"lxc","node":"dc1pve03","name":"dc1haproxy02","status":"stopped"},
		{"type":"lxc","node":"dc1pve02","name":"paused01","status":"paused"},
		{"type":"lxc","node":"dc1pve02","name":"nostatus01"},
		{"type":"lxc","node":"","name":"unplaced","status":"running"}
	]}`
	src := New("https://dc1pve01:8006", config.SecretRef("env:TG_TEST_PVE_TOKEN"), WithHTTPClient(&resDoer{body: body}))

	if _, ok := src.GuestStates(); ok {
		t.Fatal("GuestStates reported ok BEFORE any sweep — an unread cluster must be unknown, not empty")
	}
	if _, err := src.Edges(context.Background()); err != nil {
		t.Fatalf("Edges: %v", err)
	}
	states, ok := src.GuestStates()
	if !ok {
		t.Fatal("a completed sweep must report ok")
	}
	// unplaced (no node) is skipped exactly like its edge; the other five guests are observed.
	want := map[string]string{
		"n8n01": "running", "dc1nc01": "running", "dc1haproxy02": "stopped",
		"paused01": "paused", "nostatus01": "",
	}
	if len(states) != len(want) {
		t.Fatalf("observed %d states, want %d: %+v", len(states), len(want), states)
	}
	for _, st := range states {
		w, present := want[st.Guest]
		if !present {
			t.Fatalf("unexpected guest %q in states", st.Guest)
		}
		if st.Status != w {
			t.Fatalf("guest %s: status %q want %q", st.Guest, st.Status, w)
		}
		if st.Node == "" {
			t.Fatalf("guest %s: node must ride along for the projection", st.Guest)
		}
	}
}

// TestGuestStatesBeforeAnySweepIsUnknown is the TG-365 emptiness arm on the accessor itself.
func TestGuestStatesBeforeAnySweepIsUnknown(t *testing.T) {
	src := New("https://x:8006", config.SecretRef("env:TG_TEST_PVE_TOKEN"))
	if states, ok := src.GuestStates(); ok || states != nil {
		t.Fatalf("no sweep yet must read (nil, false); got (%v, %v)", states, ok)
	}
}

// TestARefusedBodyDoesNotBlankTheLastGoodStates: a malformed answer (a gateway's JSON, an SSO page) must
// not erase the previous sweep — the reader's staleness bound is what retires an old reading, not a parse
// failure minting a false "no guests".
func TestARefusedBodyDoesNotBlankTheLastGoodStates(t *testing.T) {
	t.Setenv("TG_TEST_PVE_TOKEN", "root@pam!tg=uuid")
	d := &resDoer{body: `{"data":[{"type":"lxc","node":"n1","name":"g1","status":"running"}]}`}
	src := New("https://x:8006", config.SecretRef("env:TG_TEST_PVE_TOKEN"), WithHTTPClient(d))
	if _, err := src.Edges(context.Background()); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	d.body = `{"nodata":"not a pve answer"}`
	if _, err := src.Edges(context.Background()); err == nil || !strings.Contains(err.Error(), "cluster resources") {
		t.Fatalf("the malformed body must refuse, got %v", err)
	}
	states, ok := src.GuestStates()
	if !ok || len(states) != 1 || states[0].Guest != "g1" || states[0].Status != "running" {
		t.Fatalf("the refused body blanked the last good states: ok=%v states=%+v", ok, states)
	}
}

// TestGuestStatesCarryTheFetchObservationTime (TG-496): the sweep stamps each state with the FETCH time (not
// the write time), threaded into the guest_liveness monotone upsert guard so the slow 5-min sweep cannot
// clobber the fast ~37s pve-liveness detector's fresher STOPPED on a later write.
func TestGuestStatesCarryTheFetchObservationTime(t *testing.T) {
	t.Setenv("TG_TEST_PVE_TOKEN", "root@pam!tg=uuid")
	at := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	body := `{"data":[{"type":"lxc","node":"dc1pve01","name":"g1","status":"running"}]}`
	src := New("https://x:8006", config.SecretRef("env:TG_TEST_PVE_TOKEN"),
		WithHTTPClient(&resDoer{body: body}), WithClock(func() time.Time { return at }))
	if _, err := src.Edges(context.Background()); err != nil {
		t.Fatalf("Edges: %v", err)
	}
	states, ok := src.GuestStates()
	if !ok || len(states) != 1 {
		t.Fatalf("want one state, got ok=%v states=%+v", ok, states)
	}
	if !states[0].ObservedAt.Equal(at) {
		t.Fatalf("ObservedAt = %v, want the injected fetch time %v", states[0].ObservedAt, at)
	}
}
