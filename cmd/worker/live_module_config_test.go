package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/cpconfig"
)

type fakeOverrides struct {
	vals map[string]string
	err  error
	n    int
}

func (f *fakeOverrides) Overrides(context.Context) (map[string]string, error) {
	f.n++
	return f.vals, f.err
}

const approverKey = "module.notifier.matrix.approvers"

func withKey(t *testing.T) {
	t.Helper()
	cpconfig.SetModuleKeys([]cpconfig.Key{{Name: approverKey, ConsoleWritable: true}})
	t.Cleanup(func() { cpconfig.SetModuleKeys(nil) })
}

// A saved override must reach the module. This is the whole requirement: a Save that does nothing until a
// redeploy is worse than no Save button.
func TestOverrideReachesTheModule(t *testing.T) {
	withKey(t)
	l := newLiveModuleConfig(&fakeOverrides{vals: map[string]string{approverKey: "@new:example"}}, time.Minute)
	got := l.list(approverKey, []string{"@boot:example"})
	if len(got) != 1 || got[0] != "@new:example" {
		t.Fatalf("the saved approver set did not reach the module: %v", got)
	}
}

// AN UNREGISTERED KEY IS NOT CONFIGURATION. The store is a database table; a row that should not be there
// must not become a setting. cpconfig.Resolve clamps the control plane the same way — the clamp is the
// law, enforced where the value is READ, not only where it is written.
//
// KILLING MUTATION: drop the Lookup/ConsoleWritable check in value(). RED.
func TestAnUnregisteredOrUnwritableKeyIsIgnored(t *testing.T) {
	withKey(t)
	l := newLiveModuleConfig(&fakeOverrides{vals: map[string]string{
		approverKey:               "@new:example",
		"module.notifier.x.ghost": "injected", // never registered
		"safety.may_actuate":      "true",     // law-pinned, and not a module key at all
	}}, time.Minute)

	if got := l.value("module.notifier.x.ghost", "fallback"); got != "fallback" {
		t.Errorf("an unregistered store row became configuration: %q", got)
	}
	if got := l.value("safety.may_actuate", "false"); got != "false" {
		t.Fatalf("a LAW key was honoured from the console store: %q — the estate mutation master switch "+
			"must never be settable this way", got)
	}
}

// THE FAILURE DIRECTION. If the store is unreachable the last known values stand. An empty approver set
// is not a safe default: it refuses every vote, so a database blip would silently make every approval
// poll unanswerable.
//
// KILLING MUTATION: return nil instead of l.cached on the error path. RED.
func TestStoreOutageKeepsTheLastKnownValues(t *testing.T) {
	withKey(t)
	f := &fakeOverrides{vals: map[string]string{approverKey: "@live:example"}}
	// 1ns, NOT 0: newLiveModuleConfig coerces a non-positive TTL to 10s, so passing 0 here made the
	// second read hit the cache and never reach the error path — the mutation survived because of that,
	// not because the code was right.
	l := newLiveModuleConfig(f, time.Nanosecond)
	if got := l.list(approverKey, nil); len(got) != 1 {
		t.Fatalf("setup: %v", got)
	}
	f.err = errors.New("database unreachable")
	got := l.list(approverKey, []string{"@boot:example"})
	if len(got) != 1 || got[0] != "@live:example" {
		t.Fatalf("a store outage changed the approver set to %v — an empty or reverted set during an "+
			"outage makes every approval poll unanswerable", got)
	}
}

// The cache must actually cache: the approver set is read on every inbound event, and the room routing on
// every notice. Querying Postgres per call puts the config store on the hot path of the notification lane.
func TestReadsAreCachedWithinTheTTL(t *testing.T) {
	withKey(t)
	f := &fakeOverrides{vals: map[string]string{approverKey: "@a:b"}}
	l := newLiveModuleConfig(f, time.Minute)
	for i := 0; i < 25; i++ {
		l.list(approverKey, nil)
	}
	if f.n != 1 {
		t.Fatalf("%d store queries for 25 reads — the config store is on the notification hot path", f.n)
	}
}

// An empty or whitespace override must not erase the boot value: "saved as blank" is far more likely to
// be an accident than an instruction to trust nobody.
func TestBlankOverrideFallsBackRatherThanErasing(t *testing.T) {
	withKey(t)
	l := newLiveModuleConfig(&fakeOverrides{vals: map[string]string{approverKey: "   "}}, time.Minute)
	got := l.list(approverKey, []string{"@boot:example"})
	if len(got) != 1 || got[0] != "@boot:example" {
		t.Fatalf("a blank override erased the approver set: %v", got)
	}
}

// No store at all (a deployment with no database) must behave exactly as before.
func TestNilStoreUsesBootValues(t *testing.T) {
	withKey(t)
	var l *liveModuleConfig
	if got := l.list(approverKey, []string{"@boot:example"}); len(got) != 1 || got[0] != "@boot:example" {
		t.Fatalf("a nil live config did not fall back to the boot value: %v", got)
	}
}
