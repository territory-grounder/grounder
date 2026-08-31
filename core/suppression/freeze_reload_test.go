package suppression

import (
	"sync"
	"testing"
	"time"
)

// A maintenance-window mechanism that needs a restart cannot be used during maintenance.
//
// Every FreezeWindow carries an absolute Start/End, and cmd/worker read its file exactly ONCE at boot. So
// declaring a window meant editing JSON on the box AND restarting the process that would observe it —
// itself a disruption during maintenance — and a file written once decays as its windows expire.
//
// Measured on the live worker 2026-08-06: "tier-1 gate active — 0 freeze, 0 fold(s), 0 schedule(s),
// 0 pattern(s), 0 rule(s)" while the wiring register reported "suppression.tier1: STARVED — 162 alerts
// admitted to the tier-1 chain offered, 0 alerts actually suppressed produced".

func win(scope string, start, end time.Time, reason string) FreezeWindow {
	return FreezeWindow{Scope: scope, Start: start, End: end, Reason: reason}
}

// KILLING MUTATION: delete Replace (or make it a no-op). RED — a window declared after boot never binds.
func TestAWindowDeclaredAfterBootTakesEffect(t *testing.T) {
	now := time.Date(2026, 8, 6, 14, 0, 0, 0, time.UTC)
	a := Alert{Host: "dc1gitea01", AlertRule: "Device rebooted"}

	g := &FreezeGate{} // the ordinary state: a file exists, nothing declared yet
	if _, frozen := g.Frozen(a, now); frozen {
		t.Fatal("an empty gate froze an alert — with nothing declared, everything must reach triage")
	}

	n := g.Replace([]FreezeWindow{win("dc1gitea01", now.Add(-time.Hour), now.Add(time.Hour), "kernel patch")})
	if n != 1 {
		t.Fatalf("Replace reported %d windows, want 1", n)
	}
	w, frozen := g.Frozen(a, now)
	if !frozen {
		t.Fatal("a window declared after construction did not take effect — this is the restart-required " +
			"defect: an operator declaring maintenance at 14:00 would have to wait for the next boot")
	}
	if w.Reason != "kernel patch" {
		t.Errorf("the matching window is not the declared one: %+v", w)
	}
}

// The reverse direction matters just as much: a window that is withdrawn (or a file that becomes
// unreadable, which the loader reports as NOTHING) must re-open the estate to full triage. That is the safe
// direction and the reason an unattended reload is acceptable at all.
//
// KILLING MUTATION: make Replace ignore an empty slice ("don't clobber good config with nothing"). RED —
// and that mutation is the tempting one, which is why it has its own test.
func TestWithdrawingEveryWindowReopensTriage(t *testing.T) {
	now := time.Date(2026, 8, 6, 14, 0, 0, 0, time.UTC)
	a := Alert{Host: "dc1gitea01", AlertRule: "Device rebooted"}
	g := &FreezeGate{Windows: []FreezeWindow{win("", now.Add(-time.Hour), now.Add(time.Hour), "estate-wide freeze")}}

	if _, frozen := g.Frozen(a, now); !frozen {
		t.Fatal("the fixture's estate-wide window did not freeze — this test is not exercising a freeze")
	}
	if n := g.Replace(nil); n != 0 {
		t.Fatalf("Replace(nil) reported %d windows, want 0", n)
	}
	if w, frozen := g.Frozen(a, now); frozen {
		t.Errorf("an alert is still frozen after every window was withdrawn (%+v) — a broken or emptied "+
			"freeze file must re-open the estate to triage, never keep it silently frozen", w)
	}
}

// Scope and time bounds must survive the swap unchanged — a reload that widened a window would suppress
// alerts nobody declared expected, which is the one direction this whole gate must not fail in.
func TestReplacePreservesScopeAndBounds(t *testing.T) {
	now := time.Date(2026, 8, 6, 14, 0, 0, 0, time.UTC)
	g := &FreezeGate{}
	g.Replace([]FreezeWindow{win("dc1gitea01", now.Add(-time.Hour), now.Add(time.Hour), "scoped")})

	if _, frozen := g.Frozen(Alert{Host: "dc1other01", AlertRule: "Device rebooted"}, now); frozen {
		t.Error("a host-scoped window froze a DIFFERENT host after the swap — scope was widened by the reload")
	}
	if _, frozen := g.Frozen(Alert{Host: "dc1gitea01"}, now.Add(2*time.Hour)); frozen {
		t.Error("the window froze an alert AFTER its End — the time bound was lost in the swap")
	}
	if _, frozen := g.Frozen(Alert{Host: "dc1gitea01"}, now.Add(-2*time.Hour)); frozen {
		t.Error("the window froze an alert BEFORE its Start")
	}
}

// Snapshot is the reporting seam; it must not hand out the live slice, or a caller ranging over it while a
// reload swaps underneath gets a torn read.
func TestSnapshotIsACopy(t *testing.T) {
	now := time.Date(2026, 8, 6, 14, 0, 0, 0, time.UTC)
	g := &FreezeGate{}
	g.Replace([]FreezeWindow{win("h1", now, now.Add(time.Hour), "r1")})
	snap := g.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot has %d windows, want 1", len(snap))
	}
	snap[0].Scope = "MUTATED"
	if got := g.Snapshot(); got[0].Scope != "h1" {
		t.Errorf("mutating the snapshot changed the gate's own windows (scope=%q) — Snapshot handed out the "+
			"live slice", got[0].Scope)
	}
}

// The reload runs on a ticker while the ingest path reads the gate. Without the lock this races, and a
// suppression gate that races is a gate that can miss an incident.
//
// KILLING MUTATION: remove the RWMutex from FreezeGate. RED under -race.
func TestConcurrentReloadAndReadIsSafe(t *testing.T) {
	now := time.Date(2026, 8, 6, 14, 0, 0, 0, time.UTC)
	g := &FreezeGate{}
	a := Alert{Host: "h1", AlertRule: "R"}
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() { // the reload ticker
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				g.Replace([]FreezeWindow{win("h1", now.Add(-time.Hour), now.Add(time.Hour), "on")})
			} else {
				g.Replace(nil)
			}
		}
	}()
	wg.Add(1)
	go func() { // the ingest path
		defer wg.Done()
		for i := 0; i < 20000; i++ {
			g.Frozen(a, now)
			g.Snapshot()
		}
	}()
	// The reader finishes on its own; stop the writer once it has.
	go func() { wg.Wait() }()
	for i := 0; i < 20000; i++ {
		g.Frozen(a, now)
	}
	close(stop)
	wg.Wait()
}

// A nil gate is the "no freeze file configured" case and must stay usable — every method has to tolerate it,
// or arming the chain by file path would panic on a deployment that declares none.
func TestNilGateIsSafe(t *testing.T) {
	var g *FreezeGate
	if _, frozen := g.Frozen(Alert{Host: "h"}, time.Now()); frozen {
		t.Error("a nil gate froze an alert")
	}
	if n := g.Replace([]FreezeWindow{{Scope: "h"}}); n != 0 {
		t.Errorf("Replace on a nil gate reported %d", n)
	}
	if s := g.Snapshot(); s != nil {
		t.Errorf("Snapshot on a nil gate returned %v", s)
	}
}
