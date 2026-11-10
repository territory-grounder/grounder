package main

import (
	"testing"
	"time"
)

// TG-632: the scope flags. The default (no -since) must stay the trailing estate-wide window every earlier
// release measured; everything else is a fixed window, and the misuses are usage errors rather than a report
// over an unintended population.
func TestParseScope(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	sc, err := parseScope("", "", "", false, 168*time.Hour, false, now)
	if err != nil || !sc.Since.Equal(now.Add(-168*time.Hour)) || !sc.Until.IsZero() || sc.Site != "" {
		t.Errorf("default scope = %+v, %v — want the trailing 168h estate window, open-ended", sc, err)
	}

	sc, err = parseScope("2026-11-01", "2026-11-15T06:00:00+01:00", "dc2", false, time.Hour, false, now)
	if err != nil {
		t.Fatalf("fixed site window: %v", err)
	}
	if !sc.Since.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) || !sc.Until.Equal(time.Date(2026, 11, 15, 5, 0, 0, 0, time.UTC)) || sc.Site != "dc2" {
		t.Errorf("fixed site window = %+v — a bare date is UTC midnight, an offset is honoured", sc)
	}

	sc, err = parseScope("2026-11-01", "", "", true, time.Hour, false, now)
	if err != nil || !sc.Until.IsZero() {
		t.Errorf("benchmark with -since only = %+v, %v — until stays open here; main closes it at now", sc, err)
	}

	if _, err := parseScope("2026-11-10", "2026-11-24", "", false, time.Hour, false, now); err != nil {
		t.Errorf("a scorecard (not the benchmark report) over a window reaching past now must be allowed: %v", err)
	}

	for _, c := range []struct {
		name               string
		since, until, site string
		bench, windowSet   bool
	}{
		{"undeclared site", "", "", "GR", false, false},
		{"benchmark without -since", "", "", "", true, false},
		{"benchmark with -site", "2026-11-01", "", "dc1", true, false},
		{"-until without -since", "", "2026-11-15", "", false, false},
		{"until before since", "2026-11-15", "2026-11-01", "", false, false},
		{"since in the future", "2026-12-01", "", "", false, false},
		{"unparseable", "11/01/2026", "", "", false, false},
		{"-window with -since", "2026-11-01", "", "", false, true},
		{"benchmark window not yet closed", "2026-11-10", "2026-11-24", "", true, false},
	} {
		if _, err := parseScope(c.since, c.until, c.site, c.bench, time.Hour, c.windowSet, now); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
}
