package main

import "testing"

// The TG-416 ratchet: a `completed` task owning a path that does not exist is a WARN (debt made measurable),
// but the count is PINNED so it can only shrink. Over the ceiling → FAIL; under → lower the ceiling; at → held.

func TestPhantomRatchetVerdict(t *testing.T) {
	cases := []struct {
		name             string
		phantom, ceiling int
		want             int
	}{
		{"a new phantom completion is over the ceiling → FAIL", 39, 38, -1},
		{"far over → FAIL", 100, 38, -1},
		{"held exactly at the ceiling → pass", 38, 38, 0},
		{"debt paid down by one → lower the ceiling", 37, 38, 1},
		{"all debt cleared → lower the ceiling", 0, 38, 1},
	}
	for _, c := range cases {
		if got := phantomRatchetVerdict(c.phantom, c.ceiling); got != c.want {
			t.Errorf("%s: phantomRatchetVerdict(%d,%d)=%d, want %d", c.name, c.phantom, c.ceiling, got, c.want)
		}
	}
}

func TestCountPhantomOwned(t *testing.T) {
	warn := []string{
		"010-ux-console: task T-010-1 is completed but owns frontend/x.ts, which does not exist (spec<->code " + phantomOwnedMarker + ")",
		"some unrelated warning that must not be counted",
		"020-decision-tracer: task T-020-1 is completed but owns a.go, which does not exist (spec<->code " + phantomOwnedMarker + ")",
	}
	if got := countPhantomOwned(warn); got != 2 {
		t.Fatalf("countPhantomOwned=%d, want 2 — only the phantom-marker lines count", got)
	}
	if got := countPhantomOwned(nil); got != 0 {
		t.Fatalf("countPhantomOwned(nil)=%d, want 0", got)
	}
	// The count and the emitted warning share phantomOwnedMarker precisely so they cannot drift: a warning
	// that omits the marker is invisible to the ratchet.
	if got := countPhantomOwned([]string{"task X completed but owns y, which does not exist"}); got != 0 {
		t.Fatalf("a warning without the shared marker must not be counted, got %d", got)
	}
}
