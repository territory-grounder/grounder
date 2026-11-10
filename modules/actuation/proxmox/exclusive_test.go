package proxmox

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/guestgroup"
	"github.com/territory-grounder/grounder/core/safety"
)

// TG-654: a declared exclusive group (default runnerA; partners bigB, bigC) on a RAM-tight node. Both ON
// overloads the node, and the partners are operator-managed (default OFF).

var exclusiveTestGroups = []guestgroup.Group{{Default: "runnerA", Partners: []string{"bigB", "bigC"}}}

func exclusiveCluster(runnerA, bigB, bigC string) *fakeDoer {
	return &fakeDoer{
		resources: `[{"name":"runnerA","node":"pve04","vmid":109,"type":"qemu","status":"` + runnerA + `"},` +
			`{"name":"bigB","node":"pve04","vmid":106,"type":"qemu","status":"` + bigB + `"},` +
			`{"name":"bigC","node":"pve04","vmid":1601,"type":"qemu","status":"` + bigC + `"},` +
			`{"name":"other","node":"pve04","vmid":200,"type":"lxc","status":"stopped"}]`,
		upid:       "UPID:pve04:0000ABCD:00001234:60ABCDEF:qmstart:109:root@pam:",
		exitstatus: "OK",
	}
}

func exclusiveModule(t *testing.T, f *fakeDoer, groups []guestgroup.Group) *Module {
	return New(testBaseURL, tokenRef(t), WithHTTPClient(f),
		WithMutation(safety.NewActuatingChokepoint(), []string{"runnerA", "bigB", "bigC", "other"}),
		WithExclusiveGroups(groups))
}

func TestExclusiveDefaultNotStartedWhilePartnerRuns(t *testing.T) {
	f := exclusiveCluster("stopped", "running", "stopped")
	_, err := exclusiveModule(t, f, exclusiveTestGroups).Exec(context.Background(), []string{"start", "runnerA"}, nil)
	if !errors.Is(err, ErrExclusiveGroup) {
		t.Fatalf("starting the default while a partner runs must be refused with ErrExclusiveGroup, got %v", err)
	}
	if f.sawStatusPost() {
		t.Fatalf("a refused start must never reach a status POST, saw %v", f.seenURLs)
	}
}

func TestExclusivePartnerIsNeverStarted(t *testing.T) {
	for _, partner := range []string{"bigB", "bigC"} {
		f := exclusiveCluster("stopped", "stopped", "stopped") // even with nothing running: a partner is operator-managed
		_, err := exclusiveModule(t, f, exclusiveTestGroups).Exec(context.Background(), []string{"start", partner}, nil)
		if !errors.Is(err, ErrExclusiveGroup) {
			t.Fatalf("starting partner %s must be refused with ErrExclusiveGroup, got %v", partner, err)
		}
		if f.sawStatusPost() {
			t.Fatalf("a refused partner start must never reach a status POST, saw %v", f.seenURLs)
		}
	}
}

func TestExclusiveDefaultStartsWhenNoPartnerRuns(t *testing.T) {
	f := exclusiveCluster("stopped", "stopped", "stopped")
	res, err := exclusiveModule(t, f, exclusiveTestGroups).Exec(context.Background(), []string{"start", "runnerA"}, nil)
	if err != nil {
		t.Fatalf("restoring the declared default with no partner running must proceed: %v", err)
	}
	if res.ExitCode != 0 || !f.sawPath("/api2/json/nodes/pve04/qemu/109/status/start") {
		t.Fatalf("must start the default, exit=%d saw %v", res.ExitCode, f.seenURLs)
	}
}

func TestExclusiveGroupsNeverRefuseAStop(t *testing.T) {
	// A stop is start-guest's commit-confirm inverse; it only ever moves toward a valid state.
	f := exclusiveCluster("running", "running", "stopped") // already an invalid both-ON state
	if _, err := exclusiveModule(t, f, exclusiveTestGroups).Exec(context.Background(), []string{"stop", "runnerA"}, nil); err != nil {
		t.Fatalf("a stop must never be refused by an exclusive group, got %v", err)
	}
	if !f.sawPath("/api2/json/nodes/pve04/qemu/109/status/stop") {
		t.Fatalf("the stop must actually be issued, saw %v", f.seenURLs)
	}
}

func TestExclusivePartnerNotProvablyStoppedRefusesTheDefault(t *testing.T) {
	// `unknown` (node down or partitioned) and `paused` (still holding RAM) are not evidence the partner is off.
	for _, st := range []string{"unknown", "paused", " Running "} {
		f := exclusiveCluster("stopped", st, "stopped")
		if _, err := exclusiveModule(t, f, exclusiveTestGroups).Exec(context.Background(), []string{"start", "runnerA"}, nil); !errors.Is(err, ErrExclusiveGroup) {
			t.Fatalf("a partner reading %q must refuse the default's start, got %v", st, err)
		}
		if f.sawStatusPost() {
			t.Fatalf("partner %q: no status POST may be issued, saw %v", st, f.seenURLs)
		}
	}
}

func TestExclusiveSameNamedStoppedRowCannotHideARunningPartner(t *testing.T) {
	f := &fakeDoer{
		resources: `[{"name":"runnerA","node":"pve04","vmid":109,"type":"qemu","status":"stopped"},` +
			`{"name":"bigB","node":"pve04","vmid":9001,"type":"qemu","status":"stopped","template":1},` +
			`{"name":"bigB","node":"pve03","vmid":106,"type":"qemu","status":"running"}]`,
		upid: "UPID:pve04:0000ABCD:00001234:60ABCDEF:qmstart:109:root@pam:", exitstatus: "OK",
	}
	if _, err := exclusiveModule(t, f, exclusiveTestGroups).Exec(context.Background(), []string{"start", "runnerA"}, nil); !errors.Is(err, ErrExclusiveGroup) {
		t.Fatalf("any row of a partner that is not stopped must refuse the default's start, got %v", err)
	}
}

func TestExclusiveGroupsLeaveOtherGuestsAndUndeclaredModulesUnchanged(t *testing.T) {
	f := exclusiveCluster("stopped", "running", "stopped")
	if _, err := exclusiveModule(t, f, exclusiveTestGroups).Exec(context.Background(), []string{"start", "other"}, nil); err != nil {
		t.Fatalf("a guest in no group must start as before: %v", err)
	}
	f = exclusiveCluster("stopped", "running", "stopped")
	if _, err := exclusiveModule(t, f, nil).Exec(context.Background(), []string{"start", "runnerA"}, nil); err != nil {
		t.Fatalf("with no groups declared the module must behave exactly as before: %v", err)
	}
}
