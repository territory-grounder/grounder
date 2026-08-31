// Package schedule is the vendor-neutral maintenance-window + scheduled-job model Territory Grounder
// derives from the estate's OWN scheduler (spec/019). It holds no vendor client: a read-only connector
// (modules/schedule/cronicle) maps its scheduler's events onto these types, and the actuation path consults
// the WindowGuard seam to DEFER a change that falls outside a sanctioned maintenance window (or inside a
// change-freeze) rather than rebuilding a scheduler.
//
// The load-bearing safety property is the fail-CLOSED-SAFE default (REQ-1903, INV-09): when the schedule
// cannot be read, MaintenanceWindow reports OUTSIDE-the-window — the conservative answer — so an unreadable
// scheduler makes TG MORE cautious (do not assume it is safe to actuate), never less.
//
// Provenance: [F] spec/019 · [R] "integrate the estate's scheduler, do not rebuild one" · [O] INV-09 (fail
// closed), INV-05 (re-read by id, no cached mutation).
package schedule

import "time"

// maxScanMinutes bounds every minute-bucket scan (window-containment and next-occurrence) so a pathological
// recurrence can never spin: 8 days of minutes. A window/horizon longer than this is rejected by the caller.
const maxScanMinutes = 8 * 24 * 60

// Recurrence is a scheduler-agnostic recurrence over calendar fields. Each slice is a whitelist for that
// field; an EMPTY slice means "every" (a cron `*`). It mirrors the union of common scheduler encodings
// (Cronicle's timing arrays map onto it 1:1). Weekday uses time.Weekday numbering (Sunday=0 .. Saturday=6);
// Month is 1-12; Day is day-of-month 1-31; Hour 0-23; Minute 0-59; Year is the full year. Evaluation is
// always in an explicit *time.Location so a window declared in one timezone is DST-correct.
type Recurrence struct {
	Years    []int
	Months   []int // 1-12
	Days     []int // 1-31 (day of month)
	Weekdays []int // 0-6, Sunday=0
	Hours    []int // 0-23
	Minutes  []int // 0-59
}

// Empty reports whether the recurrence constrains nothing — every field is a wildcard. An all-wildcard
// recurrence matches every minute; a caller deriving a maintenance window from one is declaring an
// always-open window and is expected to guard against it.
func (r Recurrence) Empty() bool {
	return len(r.Years) == 0 && len(r.Months) == 0 && len(r.Days) == 0 &&
		len(r.Weekdays) == 0 && len(r.Hours) == 0 && len(r.Minutes) == 0
}

// contains reports set membership, treating an empty whitelist as "every" (wildcard).
func contains(set []int, v int) bool {
	if len(set) == 0 {
		return true
	}
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

// matchesMinute reports whether an occurrence starts at minute t (t must already be in the target
// location). Every field must match; an empty field is a wildcard.
func (r Recurrence) matchesMinute(t time.Time) bool {
	return contains(r.Years, t.Year()) &&
		contains(r.Months, int(t.Month())) &&
		contains(r.Days, t.Day()) &&
		contains(r.Weekdays, int(t.Weekday())) &&
		contains(r.Hours, t.Hour()) &&
		contains(r.Minutes, t.Minute())
}

// WindowContains reports whether now falls inside SOME occurrence window [start, start+dur) of this
// recurrence, where each occurrence starts at a matching minute. It scans backward minute-by-minute over
// the interval (now-dur, now]: a matching start S in that interval necessarily has S <= now < S+dur, so
// now is inside its window. The scan is bounded by dur (capped at maxScanMinutes); loc must be non-nil.
// A non-positive dur means there is no window (an occurrence with zero length contains no instant).
func (r Recurrence) WindowContains(now time.Time, dur time.Duration, loc *time.Location) bool {
	_, _, ok := r.ActiveWindow(now, dur, loc)
	return ok
}

// ActiveWindow returns the absolute [start, end) of the single occurrence of this recurrence that contains
// now, for a window of length dur; ok is false when now is inside no occurrence. It is the projection
// primitive behind WindowContains (which is just the boolean of ok): scanning backward over (now-dur, now]
// finds the most-recent matching occurrence start S with S <= now < S+dur, and start=S, end=S+dur. A
// recurrence-based window can thus be PINNED to an absolute span (e.g. the tier-1 suppression FreezeGate,
// whose windows carry an absolute Start/End rather than a recurrence), re-derived on each reload tick rather
// than re-evaluated per read. The scan is bounded by dur (capped at maxScanMinutes); loc must be non-nil,
// and a non-positive dur is no window (a zero-length occurrence contains no instant).
func (r Recurrence) ActiveWindow(now time.Time, dur time.Duration, loc *time.Location) (start, end time.Time, ok bool) {
	if loc == nil || dur <= 0 {
		return time.Time{}, time.Time{}, false
	}
	minutes := int(dur / time.Minute)
	if dur%time.Minute != 0 {
		minutes++ // round the window up to the enclosing minute so a sub-minute tail still counts
	}
	if minutes > maxScanMinutes {
		minutes = maxScanMinutes
	}
	scanEnd := now.Truncate(time.Minute)
	lower := now.Add(-dur) // exclusive: an occurrence starting exactly dur ago ended at now (half-open)
	for m, cur := 0, scanEnd; m <= minutes && cur.After(lower); m, cur = m+1, cur.Add(-time.Minute) {
		if !cur.After(now) && cur.Add(dur).After(now) && r.matchesMinute(cur.In(loc)) {
			return cur, cur.Add(dur), true
		}
	}
	return time.Time{}, time.Time{}, false
}

// Next returns the first occurrence-start at or after now (truncated up to the next whole minute) within
// horizon, scanning forward minute-by-minute. ok is false when no occurrence falls in [now, now+horizon].
// The scan is bounded by horizon (capped at maxScanMinutes); loc must be non-nil.
func (r Recurrence) Next(now time.Time, horizon time.Duration, loc *time.Location) (time.Time, bool) {
	if loc == nil || horizon <= 0 {
		return time.Time{}, false
	}
	minutes := int(horizon / time.Minute)
	if minutes > maxScanMinutes {
		minutes = maxScanMinutes
	}
	// start at the next whole minute boundary at or after now.
	start := now.Truncate(time.Minute)
	if start.Before(now) {
		start = start.Add(time.Minute)
	}
	for m, cur := 0, start; m <= minutes; m, cur = m+1, cur.Add(time.Minute) {
		if r.matchesMinute(cur.In(loc)) {
			return cur, true
		}
	}
	return time.Time{}, false
}
