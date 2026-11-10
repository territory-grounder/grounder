package pveliveness

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/guestgroup"
)

// TG-654: an operator RAM swap on one node (default runnerA; partners bigB, bigC). Stop runnerA, start a big VM;
// later stop the big VM, start runnerA. Measured 2026-11-07: the stop-then-start gap was 20 s.

const exclusiveGrace = 5 * time.Minute

type exclusiveHarness struct {
	t    *testing.T
	f    *fakeDoer
	s    *Source
	now  time.Time
	logs []string
}

func newExclusiveHarness(t *testing.T, allowed []string, follow bool, groups []guestgroup.Group) *exclusiveHarness {
	h := &exclusiveHarness{t: t, f: &fakeDoer{}, now: time.Date(2026, 11, 7, 11, 8, 0, 0, time.UTC)}
	t.Setenv("PVE_LIVENESS_TEST_TOKEN", "env-tok")
	opts := []Option{WithHTTPClient(h.f), WithClock(func() time.Time { return h.now }),
		WithExclusiveGroups(groups, exclusiveGrace, func(f string, a ...any) { h.logs = append(h.logs, fmt.Sprintf(f, a...)) })}
	if follow {
		opts = append(opts, WithFollowCluster())
	}
	h.s = New("https://pve:8006", "env:PVE_LIVENESS_TEST_TOKEN", allowed, "NL", opts...)
	return h
}

var testGroups = []guestgroup.Group{{Default: "runnerA", Partners: []string{"bigB", "bigC"}}}

// poll advances the clock by d, serves the three members' statuses (plus an unrelated guest), and returns the
// envelopes minted.
func (h *exclusiveHarness) poll(d time.Duration, runnerA, bigB, bigC, other string) []string {
	h.t.Helper()
	h.now = h.now.Add(d)
	h.f.body = resources(qemu("runnerA", runnerA) + "," + qemu("bigB", bigB) + "," + qemu("bigC", bigC) + "," + lxc("other", other))
	got, err := h.s.FetchActive(context.Background())
	if err != nil {
		h.t.Fatalf("poll: %v", err)
	}
	var hosts []string
	for _, e := range got {
		hosts = append(hosts, e.Host)
	}
	return hosts
}

func qemu(name, status string) string {
	return `{"type":"qemu","node":"pve04","name":"` + name + `","vmid":109,"status":"` + status + `"}`
}

func (h *exclusiveHarness) logged(sub string) bool {
	for _, l := range h.logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestExclusiveSwapIsNeverRaised(t *testing.T) {
	h := newExclusiveHarness(t, nil, true, testGroups)
	if got := h.poll(0, "running", "stopped", "stopped", "running"); len(got) != 0 {
		t.Fatalf("baseline: %v", got)
	}
	// The swap: runnerA stops first (its edge must not fire), then bigB starts 20 s later.
	if got := h.poll(30*time.Second, "stopped", "stopped", "stopped", "running"); len(got) != 0 {
		t.Fatalf("the default's stop edge must mint nothing (the group check decides): %v", got)
	}
	if got := h.poll(30*time.Second, "stopped", "running", "stopped", "running"); len(got) != 0 {
		t.Fatalf("default OFF + partner ON is the declared swap, not a fault: %v", got)
	}
	if got := h.poll(time.Hour, "stopped", "running", "stopped", "running"); len(got) != 0 {
		t.Fatalf("the swapped state must never be raised, however long it lasts: %v", got)
	}
	if !h.logged("the declared swap, not a fault") {
		t.Errorf("the expected swap must be logged once, logs: %v", h.logs)
	}
}

func TestExclusiveDefaultDownWithNoPartnerIsRaisedOnceAfterGrace(t *testing.T) {
	h := newExclusiveHarness(t, nil, true, testGroups)
	h.poll(0, "running", "stopped", "stopped", "running")
	if got := h.poll(30*time.Second, "stopped", "stopped", "stopped", "running"); len(got) != 0 {
		t.Fatalf("inside the grace window nothing is raised: %v", got)
	}
	if got := h.poll(exclusiveGrace-time.Minute, "stopped", "stopped", "stopped", "running"); len(got) != 0 {
		t.Fatalf("still inside the grace window: %v", got)
	}
	got := h.poll(time.Minute, "stopped", "stopped", "stopped", "running")
	if len(got) != 1 || got[0] != "runnerA" {
		t.Fatalf("the default stopped with no partner for the whole grace window must be raised ONCE, got %v", got)
	}
	if got := h.poll(10*time.Minute, "stopped", "stopped", "stopped", "running"); len(got) != 0 {
		t.Fatalf("the same episode must not be raised again: %v", got)
	}
	// A new episode after the default came back is raised again.
	h.poll(time.Minute, "running", "stopped", "stopped", "running")
	h.poll(time.Minute, "stopped", "stopped", "stopped", "running")
	if got := h.poll(exclusiveGrace, "stopped", "stopped", "stopped", "running"); len(got) != 1 {
		t.Fatalf("a new episode must be raised again, got %v", got)
	}
}

func TestExclusivePartnerStopIsNeverRaisedAndASwapBackIsQuiet(t *testing.T) {
	h := newExclusiveHarness(t, nil, true, testGroups)
	h.poll(0, "stopped", "running", "stopped", "running") // swapped state
	if got := h.poll(30*time.Second, "stopped", "stopped", "stopped", "running"); len(got) != 0 {
		t.Fatalf("a partner's stop must never be raised (its default is OFF): %v", got)
	}
	if got := h.poll(30*time.Second, "running", "stopped", "stopped", "running"); len(got) != 0 {
		t.Fatalf("the default back within grace = the swap-back completed: %v", got)
	}
	if got := h.poll(time.Hour, "running", "stopped", "stopped", "running"); len(got) != 0 {
		t.Fatalf("default state: %v", got)
	}
}

func TestExclusiveSwapBackThatNeverHappensIsRaised(t *testing.T) {
	h := newExclusiveHarness(t, nil, true, testGroups)
	h.poll(0, "stopped", "running", "stopped", "running")
	h.poll(30*time.Second, "stopped", "stopped", "stopped", "running") // bigB stops; runnerA should come back
	got := h.poll(exclusiveGrace, "stopped", "stopped", "stopped", "running")
	if len(got) != 1 || got[0] != "runnerA" {
		t.Fatalf("the default still down a grace window after the partner stopped must be raised, got %v", got)
	}
}

func TestExclusiveGroupsLeaveOtherGuestsOnTheEdge(t *testing.T) {
	h := newExclusiveHarness(t, nil, true, testGroups)
	h.poll(0, "running", "stopped", "stopped", "running")
	if got := h.poll(30*time.Second, "running", "stopped", "stopped", "stopped"); len(got) != 1 || got[0] != "other" {
		t.Fatalf("a guest in no group must still fire on its edge, as before: %v", got)
	}
}

func TestNoExclusiveGroupsIsTodaysEdgeBehaviour(t *testing.T) {
	h := newExclusiveHarness(t, nil, true, nil)
	h.poll(0, "running", "stopped", "stopped", "running")
	if got := h.poll(30*time.Second, "stopped", "stopped", "stopped", "running"); len(got) != 1 || got[0] != "runnerA" {
		t.Fatalf("with no groups declared the default's stop must fire on its edge exactly as today, got %v", got)
	}
	if len(h.logs) != 0 {
		t.Fatalf("no groups declared ⇒ no policy log lines, got %v", h.logs)
	}
}

func TestExclusiveDefaultAlreadyDownAtStartIsRaisedAfterGrace(t *testing.T) {
	// Booting INTO the anomaly (e.g. after an outage, before the swap-back ran): the declared default is ON, so a
	// default found stopped with no partner running is the one state-based raise, bounded to declared members.
	h := newExclusiveHarness(t, nil, true, testGroups)
	if got := h.poll(0, "stopped", "stopped", "stopped", "stopped"); len(got) != 0 {
		t.Fatalf("first poll arms, never raises: %v", got)
	}
	if got := h.poll(exclusiveGrace, "stopped", "stopped", "stopped", "stopped"); len(got) != 1 || got[0] != "runnerA" {
		t.Fatalf("the default down at boot with no partner must be raised after grace (and nothing else), got %v", got)
	}
}

func TestExclusivePartnerNotProvablyStoppedIsNotAnAnomaly(t *testing.T) {
	// A partner reading `unknown` (its node down or partitioned) or `paused` may still be holding the RAM: the
	// default being down is then not "the swap did not complete", and raising it would heal into both-ON.
	for _, st := range []string{"unknown", "paused"} {
		h := newExclusiveHarness(t, nil, true, testGroups)
		h.poll(0, "running", "stopped", "stopped", "running")
		h.poll(30*time.Second, "stopped", st, "stopped", "running")
		if got := h.poll(exclusiveGrace+time.Minute, "stopped", st, "stopped", "running"); len(got) != 0 {
			t.Fatalf("partner %q: the default must not be raised, got %v", st, got)
		}
	}
}

func TestExclusiveSwapLoggedExactlyOnce(t *testing.T) {
	h := newExclusiveHarness(t, nil, true, testGroups)
	h.poll(0, "running", "stopped", "stopped", "running")
	h.poll(30*time.Second, "stopped", "stopped", "stopped", "running")
	for range 5 {
		h.poll(time.Minute, "stopped", "running", "stopped", "running")
	}
	n := 0
	for _, l := range h.logs {
		if strings.Contains(l, "the declared swap, not a fault") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the swap must be logged exactly once per episode, got %d: %v", n, h.logs)
	}
}

func TestExclusiveUnwatchedDefaultIsNeverRaised(t *testing.T) {
	h := newExclusiveHarness(t, []string{"other"}, false, testGroups) // static allowlist without runnerA
	h.poll(0, "running", "stopped", "stopped", "running")
	h.poll(30*time.Second, "stopped", "stopped", "stopped", "running")
	if got := h.poll(exclusiveGrace, "stopped", "stopped", "stopped", "running"); len(got) != 0 {
		t.Fatalf("a default outside the watched set must never be raised: %v", got)
	}
}
