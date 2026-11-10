package main

import (
	"strings"
	"testing"
)

// TG-584: a trend run judged under a different rubric version than the committed anchor says so out loud; the
// same version, or an unversioned side, says nothing (pre-versioning anchors are not a crossing).
func TestRubricCrossingIsLoudOnlyWhenVersionsDiffer(t *testing.T) {
	for _, tc := range []struct {
		base, run string
		crossed   bool
	}{
		{"2026-08-04.3", "2026-10-23.1", true},
		{"2026-10-23.1", "2026-10-23.1", false},
		{"", "2026-10-23.1", false},
		{"2026-08-04.3", "", false},
	} {
		msg, crossed := rubricCrossing(tc.base, tc.run)
		if crossed != tc.crossed {
			t.Fatalf("%q vs %q: crossed=%v want %v", tc.base, tc.run, crossed, tc.crossed)
		}
		if crossed && (!strings.Contains(msg, tc.base) || !strings.Contains(msg, tc.run) || !strings.Contains(msg, "INSTRUMENT")) {
			t.Fatalf("the crossing message must name both versions and say it measures the instrument: %q", msg)
		}
	}
}
