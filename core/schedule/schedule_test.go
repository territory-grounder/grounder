package schedule

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/safety"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load location %q: %v", name, err)
	}
	return loc
}

// nightlyMaint is the "Nightly maintenance window (librespeed01)" demo event: 02:00 daily, 3h, Amsterdam.
func nightlyMaint(loc *time.Location) WindowRule {
	return WindowRule{
		Kind: KindMaintenance, Target: "librespeed01", Title: "Nightly maintenance window", EventID: "ev-maint",
		Rec: Recurrence{Hours: []int{2}, Minutes: []int{0}}, Duration: 3 * time.Hour, Loc: loc,
	}
}

// moratorium is the "Nightly change moratorium" demo freeze: 03:00 daily, 1h, estate-wide.
func moratorium(loc *time.Location) WindowRule {
	return WindowRule{
		Kind: KindFreeze, Target: "*", Title: "Nightly change moratorium", EventID: "ev-freeze",
		Rec: Recurrence{Hours: []int{3}, Minutes: []int{0}}, Duration: time.Hour, Loc: loc,
	}
}

func TestRecurrenceWindowContains(t *testing.T) {
	loc := mustLoc(t, "Europe/Amsterdam")
	rec := Recurrence{Hours: []int{2}, Minutes: []int{0}} // 02:00 daily
	dur := 3 * time.Hour                                  // window [02:00, 05:00)
	at := func(h, m int) time.Time { return time.Date(2026, 7, 20, h, m, 0, 0, loc) }
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"start edge inside", at(2, 0), true},
		{"mid window", at(3, 30), true},
		{"one min before end", at(4, 59), true},
		{"end edge exclusive", at(5, 0), false},
		{"before start", at(1, 59), false},
		{"well after", at(9, 0), false},
	}
	for _, c := range cases {
		if got := rec.WindowContains(c.now, dur, loc); got != c.want {
			t.Errorf("%s: WindowContains(%s)=%v want %v", c.name, c.now.Format("15:04"), got, c.want)
		}
	}
}

func TestRecurrenceNext(t *testing.T) {
	loc := mustLoc(t, "Europe/Amsterdam")
	rec := Recurrence{Minutes: []int{15}} // every hour at :15
	now := time.Date(2026, 7, 20, 13, 20, 0, 0, loc)
	next, ok := rec.Next(now, 2*time.Hour, loc)
	if !ok {
		t.Fatal("expected an imminent occurrence")
	}
	if want := time.Date(2026, 7, 20, 14, 15, 0, 0, loc); !next.Equal(want) {
		t.Fatalf("next=%s want %s", next.Format("15:04"), want.Format("15:04"))
	}
	// no occurrence in a sub-15-minute horizon just after :15.
	if _, ok := rec.Next(time.Date(2026, 7, 20, 14, 16, 0, 0, loc), 10*time.Minute, loc); ok {
		t.Fatal("did not expect an occurrence within 10m after :15")
	}
}

func TestMaintenanceWindowStates(t *testing.T) {
	loc := mustLoc(t, "Europe/Amsterdam")
	cal := Calendar{Readable: true, Source: "test", Windows: []WindowRule{nightlyMaint(loc), moratorium(loc)}}
	at := func(h, m int) time.Time { return time.Date(2026, 7, 20, h, m, 0, 0, loc) }

	// inside maintenance, before the freeze carve-out -> sanctioned.
	if in, reason := cal.MaintenanceWindow("librespeed01", at(2, 30)); !in {
		t.Errorf("02:30 librespeed01 should be in-window, got false: %s", reason)
	}
	// inside the freeze carve-out (which also overlaps maintenance) -> freeze overrides -> NOT sanctioned.
	if in, reason := cal.MaintenanceWindow("librespeed01", at(3, 30)); in {
		t.Errorf("03:30 librespeed01 is under a change-freeze; want NOT in-window, got true: %s", reason)
	}
	// outside every window -> not sanctioned.
	if in, _ := cal.MaintenanceWindow("librespeed01", at(12, 0)); in {
		t.Error("12:00 should be outside any window")
	}
	// a host the maintenance window does not scope to -> not sanctioned even mid-window.
	if in, _ := cal.MaintenanceWindow("dc1tg01", at(2, 30)); in {
		t.Error("dc1tg01 is out of the librespeed01 maintenance scope; want not in-window")
	}
}

func TestMaintenanceWindowFailClosedWhenUnreadable(t *testing.T) {
	cal := Calendar{Readable: false, Source: "test", Note: "scheduler unreachable"}
	in, reason := cal.MaintenanceWindow("librespeed01", time.Now())
	if in {
		t.Fatal("an unreadable schedule MUST report OUTSIDE the window (fail closed safe)")
	}
	if reason != reasonUnreadable {
		t.Fatalf("unexpected reason: %q", reason)
	}
	// the conservative default clamps to POLL_PAUSE.
	band, in2, _ := cal.EvaluateBand("librespeed01", time.Now())
	if in2 || band != safety.BandPollPause {
		t.Fatalf("unreadable must clamp to POLL_PAUSE, got band=%s in=%v", band, in2)
	}
}

func TestEvaluateBandInWindowIsAuto(t *testing.T) {
	loc := mustLoc(t, "Europe/Amsterdam")
	cal := Calendar{Readable: true, Windows: []WindowRule{nightlyMaint(loc)}}
	band, in, _ := cal.EvaluateBand("librespeed01", time.Date(2026, 7, 20, 2, 30, 0, 0, loc))
	if !in || band != safety.BandAuto {
		t.Fatalf("in-window should not clamp, got band=%s in=%v", band, in)
	}
}

func TestImminentJob(t *testing.T) {
	loc := mustLoc(t, "Europe/Amsterdam")
	cal := Calendar{Readable: true, Jobs: []ScheduledJob{{
		Title: "Hourly log rotation", EventID: "ev-job", Target: "*",
		Rec: Recurrence{Minutes: []int{15}}, Loc: loc,
	}}}
	now := time.Date(2026, 7, 20, 13, 50, 0, 0, loc)
	job, next, ok := cal.ImminentJob("librespeed01", now, time.Hour)
	if !ok || job.EventID != "ev-job" {
		t.Fatalf("expected the hourly job imminent within 1h, got ok=%v job=%+v", ok, job)
	}
	if want := time.Date(2026, 7, 20, 14, 15, 0, 0, loc); !next.Equal(want) {
		t.Fatalf("next=%s want %s", next, want)
	}
}

func TestParseDirective(t *testing.T) {
	cases := []struct {
		in      string
		present bool
		kind    WindowKind
		dur     time.Duration
		target  string
	}{
		{"tg-window=maintenance tg-duration=3h tg-target=librespeed01\nnotes", true, KindMaintenance, 3 * time.Hour, "librespeed01"},
		{"tg-window=freeze tg-duration=1h tg-target=*", true, KindFreeze, time.Hour, "*"},
		{"tg-window", true, KindMaintenance, 0, ""},
		{"no directive here", false, KindUnspecified, 0, ""},
		{"tg-window=bogus", true, KindUnspecified, 0, ""},
		{"tg-window=maint,tg-duration=90m", true, KindMaintenance, 90 * time.Minute, ""},
	}
	for _, c := range cases {
		d := ParseDirective(c.in)
		if d.Present != c.present || d.Kind != c.kind || d.Duration != c.dur || d.Target != c.target {
			t.Errorf("ParseDirective(%q) = %+v; want present=%v kind=%v dur=%v target=%q",
				c.in, d, c.present, c.kind, c.dur, c.target)
		}
	}
}

func TestMatchTarget(t *testing.T) {
	cases := []struct {
		pattern, target string
		want            bool
	}{
		{"*", "anything", true},
		{"", "anything", true},
		{"librespeed01", "librespeed01", true},
		{"librespeed01", "myspeed01", false},
		{"dc1*", "dc1tg01", true},
		{"dc1*", "dc2fw01", false},
	}
	for _, c := range cases {
		if got := matchTarget(c.pattern, c.target); got != c.want {
			t.Errorf("matchTarget(%q,%q)=%v want %v", c.pattern, c.target, got, c.want)
		}
	}
}
