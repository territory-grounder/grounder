package suppression

import (
	"sync"
	"time"
)

// FreezeWindow is a declared maintenance / chaos-drill freeze: for its span, the alerts its scope is
// EXPECTED to produce are not turned into remediation sessions. Scope is host-, rule-, or estate-level; an
// empty Scope covers the whole estate. A freeze is a deliberate operator declaration, so — unlike the
// ordinary suppression phases — it may suppress an EXPECTED critical alert (a reboot's HostDown), because
// the operator already knows it is coming. It is deliberately narrow: only alerts matching a declared
// window's scope are frozen; everything else, including an unexpected critical, still escalates.
type FreezeWindow struct {
	Scope  string // "" = whole estate; else the host or alert_rule the window covers
	Start  time.Time
	End    time.Time
	Reason string
}

// FreezeGate holds the currently-declared freeze windows. It is consulted BEFORE the severity floor, so a
// scoped, active window suppresses the expected alert regardless of severity (the predecessor's pre-chain
// maintenance/chaos freeze state, PORT-FIDELITY-AUDIT P0-6 + the external audit's chaos-freeze recommendation).
// ★ AND IT MUST BE REPLACEABLE WITHOUT A RESTART (2026-08-06). Every freeze window carries an absolute
// Start/End, and the worker read its file exactly ONCE at boot — so declaring a maintenance window meant
// editing JSON on the box AND restarting the very process that would observe it. Restarting the worker is
// itself a disruption during maintenance, and a file written once decays: its windows expire and nothing
// reloads to admit new ones.
//
// That is a maintenance-window mechanism that cannot be used for maintenance, and it is the mechanism-level
// reason the plane sat empty. Measured on the live worker: "tier-1 gate active — 0 freeze, 0 fold(s),
// 0 schedule(s), 0 pattern(s), 0 rule(s)" while the wiring register reported "suppression.tier1: STARVED —
// 162 alerts admitted to the tier-1 chain offered, 0 alerts actually suppressed produced".
//
// Windows are therefore held behind a mutex and swapped by Replace. Reads take a read-lock; the zero value
// is still a usable gate with no windows, so existing construction (`&FreezeGate{Windows: w}`) keeps
// working unchanged.
type FreezeGate struct {
	mu sync.RWMutex
	// Windows is the initial set. After construction, read it through Snapshot and write it through
	// Replace — direct field access after a Replace races.
	Windows []FreezeWindow
}

// Replace swaps the declared windows atomically and reports how many are now held. It is the reload seam:
// a periodic re-read of the operator's freeze file calls this, so a window declared at 14:00 is in force at
// 14:00 and not at the next restart.
//
// An EMPTY set is a legitimate replacement — it means "no freeze is declared", which is the SAFE direction:
// nothing is suppressed and every alert reaches triage. That matters because the loader deliberately
// returns nothing for an unreadable or malformed file, so a broken file re-opens the estate to full triage
// rather than silently freezing it.
func (g *FreezeGate) Replace(w []FreezeWindow) int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Windows = w
	return len(g.Windows)
}

// Snapshot returns a copy of the currently-declared windows, for reporting.
func (g *FreezeGate) Snapshot() []FreezeWindow {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]FreezeWindow, len(g.Windows))
	copy(out, g.Windows)
	return out
}

// Frozen reports whether an alert falls inside an active, in-scope freeze window (and the matching window).
func (g *FreezeGate) Frozen(a Alert, now time.Time) (FreezeWindow, bool) {
	if g == nil {
		return FreezeWindow{}, false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, w := range g.Windows {
		if now.Before(w.Start) || now.After(w.End) {
			continue // window not active
		}
		if w.Scope == "" || w.Scope == a.Host || w.Scope == a.AlertRule {
			return w, true
		}
	}
	return FreezeWindow{}, false
}
