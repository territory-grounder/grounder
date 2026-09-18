package main

// TG-378 wiring guards (the reviewer's "implemented ≠ reachable" advisory on slice 1): the feed and the
// gate seam exist only if main.go actually CALLS them. These are source assertions in the
// may_actuate_published_test.go house pattern — the closures are built inline in main() with no seam to
// invoke, so the test pins the call sites, with a vacuity floor and a stripper self-test so prose cannot
// satisfy them.
//
// KILLING MUTATION (executed 2026-08-11): delete the `feedLiveness(context.Background(), "refresh tick", false)`
// line from the estate ticker — TestGuestLivenessCallSitesAreWired fails naming the tick site. Restore →
// green. (A dropped call site fails SAFE — the projection ages to unknown and the precondition refuses —
// but silently-refusing-everything is a dead capability, the exact "present, not reaching" shape TG-378
// was filed about.)

import (
	"os"
	"strings"
	"testing"
)

func workerMainSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		out = append(out, line)
	}
	src := strings.Join(out, "\n")
	if len(src) < 10_000 {
		t.Fatalf("VACUITY FLOOR: main.go stripped to %d bytes — every assertion below would pass on a stub", len(src))
	}
	return src
}

func TestWorkerSourceStripperActuallyStrips(t *testing.T) {
	src := "// feedLiveness(context.Background(), \"x\", false)\nreal()\n"
	var out []string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		out = append(out, line)
	}
	if strings.Contains(strings.Join(out, "\n"), "feedLiveness") {
		t.Fatal("the stripper left a comment in place — prose could satisfy every assertion below")
	}
}

func TestGuestLivenessCallSitesAreWired(t *testing.T) {
	src := workerMainSource(t)
	for site, want := range map[string]string{
		"boot prime":   `feedLiveness(context.Background(), "boot prime", true)`,
		"refresh tick": `feedLiveness(context.Background(), "refresh tick", false)`,
		"store armed":  `guestLivenessStore.Store(db.NewGuestLivenessStore(pool))`,
		"pve source":   `pveGuestSource = pveSrc`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the %s call site is GONE from main.go — the projection silently stops being fed/armed "+
				"and every reading ages to unknown (a dead capability wearing fail-closed clothes): missing %q", site, want)
		}
	}
}

func TestTheGateStateSeamIsWired(t *testing.T) {
	src := workerMainSource(t)
	if !strings.Contains(src, `GuestRunning: guestRunningReader(guestLivenessStore.Load(), guestLivenessBound)`) {
		t.Error("the prediction gate's GuestRunning seam is not wired from the guest_liveness projection — " +
			"the gate then refuses every state-preconditioned class forever (fail-closed but dead, TG-378)")
	}
}
