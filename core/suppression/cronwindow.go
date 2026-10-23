package suppression

import (
	"strconv"
	"strings"
	"time"
)

// WindowEvaluator decides whether an alert time falls within a schedule's cron window, timezone-aware
// and DST-correct: it evaluates the alert time IN the schedule's location, so an hour named in local
// time lands correctly across a DST transition. It re-implements a real cron matcher — never a
// shelled-out croniter (INV-02 keeps shell out of the tree anyway).
type WindowEvaluator struct {
	// The window around each scheduled fire is ASYMMETRIC: [fire − PreBuffer, fire + PostWindow]. A reboot
	// alert normally arrives AFTER the fire (detection lag + the reboot itself), so the post-window is wider
	// than the pre-buffer — mirroring the predecessor's pre_buffer(5m)/window(10m). A symmetric ±tolerance
	// cannot express this: widening it to catch a fire+8m boot would also wrongly suppress a fire−8m boot.
	PreBuffer  time.Duration // how long BEFORE a scheduled fire an alert still matches (predecessor pre_buffer, default 5m)
	PostWindow time.Duration // how long AFTER  a scheduled fire an alert still matches (predecessor window,     default 10m)
}

// Contains reports whether t falls within sc's window. It returns false (⇒ no match ⇒ the chain
// escalates, fail open) on a bad timezone or an unparseable cron.
func (w WindowEvaluator) Contains(sc Schedule, t time.Time) bool {
	loc, err := time.LoadLocation(sc.Timezone)
	if err != nil || loc == nil {
		return false
	}
	spec, ok := parseCron(sc.Cron)
	if !ok {
		return false
	}
	lt := t.In(loc)
	// Evaluate the fires on lt's day AND the adjacent days, so a boot just after midnight matches a
	// late-night cron from the PREVIOUS day (e.g. `59 23 * * *` fires 23:59, a boot at 00:03 is a match) —
	// the same-day-only evaluation missed every cross-midnight window. Day matching (DOM/month/DOW) is checked
	// on the FIRE's day, so a Sunday 23:59 schedule matches a Monday-00:03 boot. Each allowed (hour, minute)
	// the cron fires at is checked against the ASYMMETRIC [fire−PreBuffer, fire+PostWindow] window; a reboot
	// cron's fire set is small.
	for _, off := range []int{0, -1, 1} {
		day := lt.AddDate(0, 0, off)
		if !spec.dayMatches(day) {
			continue
		}
		for h := range spec.hour {
			for m := range spec.minute {
				sched := time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, loc)
				delta := lt.Sub(sched) // signed: >0 ⇒ the alert is AFTER the fire, <0 ⇒ before
				if delta >= -w.PreBuffer && delta <= w.PostWindow {
					return true
				}
			}
		}
	}
	return false
}

// cronSpec is a parsed 5-field cron expression as per-field sets of allowed values. A single value, `*`, a
// range `a-b`, a step `*/s` or `a-b/s`, and comma-lists of these are all supported.
type cronSpec struct {
	minute, hour, dom, month, dow fieldSet
	domRestricted, dowRestricted  bool // whether day-of-month / day-of-week are constrained (not `*`)
}

// fieldSet is the set of integer values a cron field matches.
type fieldSet map[int]bool

func (fs fieldSet) has(v int) bool { return fs[v] }

// dayMatches applies cron day semantics for a candidate fire day: the month must match, and — following the
// classic crontab rule — when BOTH day-of-month and day-of-week are constrained the day matches if EITHER
// does; when only one is constrained only that field applies; when neither is constrained (both `*`) any day
// matches. Weekday is 0=Sunday.
func (s cronSpec) dayMatches(day time.Time) bool {
	if !s.month.has(int(day.Month())) {
		return false
	}
	dom := s.dom.has(day.Day())
	dow := s.dow.has(int(day.Weekday()))
	switch {
	case s.domRestricted && s.dowRestricted:
		return dom || dow
	case s.domRestricted:
		return dom
	case s.dowRestricted:
		return dow
	default:
		return true
	}
}

// parseCron parses a full 5-field "M H DOM MON DOW" cron into per-field sets. It supports `*`, a single
// value, a range `a-b`, a step `*/s` or `a-b/s`, and comma-lists of these — the real crontab grammar a
// scheduled reboot may use (a weekday range `1-5`, a set `1,3,5`, a monthly `1` day-of-month). A field or
// value outside its range, or any malformed token, fails the parse (ok=false ⇒ no suppression, fail open).
func parseCron(cron string) (cronSpec, bool) {
	f := strings.Fields(cron)
	if len(f) != 5 {
		return cronSpec{}, false
	}
	minute, ok1 := parseField(f[0], 0, 59)
	hour, ok2 := parseField(f[1], 0, 23)
	dom, ok3 := parseField(f[2], 1, 31)
	month, ok4 := parseField(f[3], 1, 12)
	dow, ok5 := parseField(f[4], 0, 7) // allow 7 as Sunday, folded to 0 below
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
		return cronSpec{}, false
	}
	if dow[7] { // both 0 and 7 denote Sunday
		dow[0] = true
		delete(dow, 7)
	}
	return cronSpec{
		minute: minute, hour: hour, dom: dom, month: month, dow: dow,
		domRestricted: f[2] != "*", dowRestricted: f[4] != "*",
	}, true
}

// parseField parses one cron field into the set of allowed values within [lo, hi]. It handles `*`, `N`,
// `a-b`, `a-b/s`, `*/s`, and comma-lists of these. An out-of-range bound, a non-positive step, an inverted
// range, or a non-numeric token makes it fail.
func parseField(field string, lo, hi int) (fieldSet, bool) {
	set := fieldSet{}
	for _, part := range strings.Split(field, ",") {
		rng := part
		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			s, err := strconv.Atoi(part[i+1:])
			if err != nil || s <= 0 {
				return nil, false
			}
			step = s
			rng = part[:i]
		}
		var start, end int
		switch {
		case rng == "*":
			start, end = lo, hi
		case strings.Contains(rng, "-"):
			ab := strings.SplitN(rng, "-", 2)
			a, ea := strconv.Atoi(ab[0])
			b, eb := strconv.Atoi(ab[1])
			if ea != nil || eb != nil {
				return nil, false
			}
			start, end = a, b
		default:
			v, err := strconv.Atoi(rng)
			if err != nil {
				return nil, false
			}
			start, end = v, v
		}
		if start < lo || end > hi || start > end {
			return nil, false
		}
		for v := start; v <= end; v += step {
			set[v] = true
		}
	}
	if len(set) == 0 {
		return nil, false
	}
	return set, true
}
