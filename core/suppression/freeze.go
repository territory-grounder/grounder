package suppression

import "time"

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
type FreezeGate struct {
	Windows []FreezeWindow
}

// Frozen reports whether an alert falls inside an active, in-scope freeze window (and the matching window).
func (g *FreezeGate) Frozen(a Alert, now time.Time) (FreezeWindow, bool) {
	if g == nil {
		return FreezeWindow{}, false
	}
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
