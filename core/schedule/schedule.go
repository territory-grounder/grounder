package schedule

import (
	"context"
	"path"
	"sort"
	"time"

	"github.com/territory-grounder/grounder/core/safety"
)

// WindowRule is one derived, recurring time window scoped to an estate target. It is scheduler-agnostic:
// the connector fills it from an event's recurrence, timezone, and operator directive. A concrete
// occurrence is [start, start+Duration) for each recurrence match, evaluated in Loc.
type WindowRule struct {
	Kind     WindowKind
	Target   string // host or glob; "" or "*" = whole estate
	Title    string // the source event's human title (non-secret)
	EventID  string // the source event id (for re-read-by-id / audit)
	Rec      Recurrence
	Duration time.Duration
	Loc      *time.Location
}

// activeAt reports whether now falls inside an occurrence of this window.
func (w WindowRule) activeAt(now time.Time) bool { return w.Rec.WindowContains(now, w.Duration, w.Loc) }

// matches reports whether this window's target scope covers the queried target. An empty or "*" scope
// covers the whole estate; otherwise a glob match (path.Match semantics — hostnames carry no '/').
func (w WindowRule) matches(target string) bool { return matchTarget(w.Target, target) }

// ScheduledJob is an already-scheduled recurring change TG should defer to / avoid colliding with. Every
// enabled scheduler event with a recurrence becomes one, whether or not it is tagged as a window.
type ScheduledJob struct {
	Title   string
	EventID string
	Target  string
	Rec     Recurrence
	Loc     *time.Location
}

// matchTarget matches a scope pattern against a concrete target. "" / "*" match everything; otherwise
// path.Match (a malformed pattern matches nothing — fail closed).
func matchTarget(pattern, target string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	if pattern == target {
		return true
	}
	ok, err := path.Match(pattern, target)
	return err == nil && ok
}

// Calendar is an immutable, derived snapshot of the estate schedule: the maintenance/freeze windows and the
// already-scheduled jobs, plus whether the snapshot could actually be READ. It answers the actuation-facing
// question "is now inside a sanctioned maintenance window for this target?" with a fail-closed-safe default.
type Calendar struct {
	Windows  []WindowRule
	Jobs     []ScheduledJob
	Readable bool   // false => the schedule could not be read; MaintenanceWindow is conservative (REQ-1903)
	Source   string // e.g. "cronicle:demo01" (non-secret provenance)
	Note     string // non-secret diagnostic (e.g. an unreadable-schedule reason class)
}

// Reason strings are stable so callers/tests can assert them without string-fragility.
const (
	reasonUnreadable = "schedule unreadable — conservative default: treat now as OUTSIDE any sanctioned maintenance window"
	reasonOutside    = "outside any sanctioned maintenance window"
)

// MaintenanceWindow reports whether now is inside a sanctioned maintenance window for target, with a
// human-readable reason. It is the seam the actuation interceptor / policy path consults to DEFER
// (POLL_PAUSE) an actuation that is not time-sanctioned. Semantics (deny-overrides):
//
//   - schedule unreadable            -> (false, unreadable)  [REQ-1903 fail-closed-safe]
//   - inside a matching FREEZE window -> (false, freeze)     [REQ-1904 freeze overrides]
//   - inside a matching MAINTENANCE window and no freeze -> (true, maintenance) [REQ-1905]
//   - otherwise (no window covers now) -> (false, outside)   [REQ-1905]
//
// The conservative default is false: absence of a sanctioned window is NOT permission to actuate.
func (c Calendar) MaintenanceWindow(target string, now time.Time) (inWindow bool, reason string) {
	if !c.Readable {
		return false, reasonUnreadable
	}
	// deny-overrides: any active, in-scope freeze wins over any maintenance window.
	for _, w := range c.Windows {
		if w.Kind == KindFreeze && w.matches(target) && w.activeAt(now) {
			return false, "inside change-freeze window " + label(w) + " — actuation not sanctioned"
		}
	}
	for _, w := range c.Windows {
		if w.Kind == KindMaintenance && w.matches(target) && w.activeAt(now) {
			return true, "inside sanctioned maintenance window " + label(w)
		}
	}
	return false, reasonOutside
}

// label renders a window's non-secret identity for a reason string.
func label(w WindowRule) string {
	scope := w.Target
	if scope == "" {
		scope = "*"
	}
	return "'" + w.Title + "' (target " + scope + ", event " + w.EventID + ")"
}

// ImminentJob reports the soonest already-scheduled job matching target whose next occurrence falls within
// horizon of now — the collision-avoidance signal (a change is already coming; do not race it). It returns
// the job, its next occurrence, and whether one is imminent. On an unreadable calendar it returns false
// (the caller already fails closed on MaintenanceWindow; collision detection is advisory).
func (c Calendar) ImminentJob(target string, now time.Time, horizon time.Duration) (job ScheduledJob, next time.Time, imminent bool) {
	if !c.Readable {
		return ScheduledJob{}, time.Time{}, false
	}
	var best time.Time
	for _, j := range c.Jobs {
		if !matchTarget(j.Target, target) {
			continue
		}
		n, ok := j.Rec.Next(now, horizon, j.Loc)
		if !ok {
			continue
		}
		if !imminent || n.Before(best) {
			best, job, imminent = n, j, true
		}
	}
	return job, best, imminent
}

// Windows/Jobs of a given kind, sorted by title — a stable read surface for the console/logs.
func (c Calendar) MaintenanceWindows() []WindowRule { return c.windowsOfKind(KindMaintenance) }
func (c Calendar) FreezeWindows() []WindowRule      { return c.windowsOfKind(KindFreeze) }

func (c Calendar) windowsOfKind(k WindowKind) []WindowRule {
	var out []WindowRule
	for _, w := range c.Windows {
		if w.Kind == k {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out
}

// WindowGuard is the actuation-facing seam: given a target and the current time, is actuation inside a
// sanctioned maintenance window? A read-only scheduler connector (modules/schedule/cronicle) implements it
// by RE-READING the live schedule (INV-05, no cached mutation) and answering through a derived Calendar; an
// unreadable schedule yields the conservative OUTSIDE answer (REQ-1903). The interceptor MAY consult it to
// clamp an out-of-window actuation to DeferBand (POLL_PAUSE). A nil WindowGuard means the gate is not wired
// and imposes no opinion — window-gating is opt-in per deployment.
type WindowGuard interface {
	MaintenanceWindow(ctx context.Context, target string, now time.Time) (inWindow bool, reason string)
}

// DeferBand is the safety band an actuation is clamped to when MaintenanceWindow reports it is NOT
// time-sanctioned (outside a maintenance window, or inside a freeze): POLL_PAUSE — hold for a human, never
// auto-execute. It is the mapping the interceptor uses to realise "defer" without this package importing the
// interceptor. POLL_PAUSE is the zero value of safety.Band (the most restrictive), so the mapping is itself
// fail-closed.
func DeferBand() safety.Band { return safety.BandPollPause }

// EvaluateBand is a convenience for a caller that wants the clamped band directly: it returns AUTO when
// in-window (no clamp) and DeferBand (POLL_PAUSE) otherwise, alongside the reason. It performs no I/O — the
// caller has already produced the Calendar (or consulted a live WindowGuard).
func (c Calendar) EvaluateBand(target string, now time.Time) (band safety.Band, inWindow bool, reason string) {
	inWindow, reason = c.MaintenanceWindow(target, now)
	if inWindow {
		return safety.BandAuto, true, reason
	}
	return DeferBand(), false, reason
}
