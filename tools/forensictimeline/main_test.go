package main

import (
	"strings"
	"testing"
	"time"
)

// TG-168 — the window flags are where an operator's mistake becomes a wrong answer, so they refuse
// rather than guess.

// THE LOAD-BEARING REFUSAL. In an incident, at the moment an operator is least able to notice, a
// defaulted window would answer a different question from the one asked — and it would look like an
// answer.
func TestNoWindowIsRefusedRatherThanDefaulted(t *testing.T) {
	_, err := window("", "", "")
	if err == nil {
		t.Fatal("no -from and no -since produced a window — a defaulted reconstruction answers a question " +
			"the operator did not ask, and an unbounded one is a dump over ~9,700 ledger rows")
	}
	if !strings.Contains(err.Error(), "will not") {
		t.Errorf("the refusal must say it is deliberate, got %q", err)
	}
}

func TestSinceIsShorthandForARelativeStart(t *testing.T) {
	w, err := window("", "", "3h")
	if err != nil {
		t.Fatalf("-since 3h: %v", err)
	}
	age := time.Since(w.From)
	if age < 2*time.Hour+50*time.Minute || age > 3*time.Hour+10*time.Minute {
		t.Errorf("-since 3h produced a start %v ago, want ~3h", age)
	}
	if !w.To.IsZero() {
		t.Error("-since must leave To open (everything since), not close it")
	}
}

// -from wins over -since, or an operator passing both gets a window neither flag describes.
func TestAnExplicitFromBeatsSince(t *testing.T) {
	w, err := window("2026-08-06T02:00:00Z", "", "3h")
	if err != nil {
		t.Fatalf("both flags: %v", err)
	}
	if w.From.Format(time.RFC3339) != "2026-08-06T02:00:00Z" {
		t.Errorf("From = %s, want the explicit -from", w.From.Format(time.RFC3339))
	}
}

// A malformed timestamp is refused, not silently coerced — a window nobody typed is worse than an error.
func TestMalformedTimestampsAreRefused(t *testing.T) {
	for _, tc := range []struct{ from, to, since string }{
		{"yesterday", "", ""},
		{"2026-08-06T02:00:00Z", "tomorrow", ""},
		{"", "", "-3h"},
		{"", "", "soon"},
	} {
		if _, err := window(tc.from, tc.to, tc.since); err == nil {
			t.Errorf("window(%q,%q,%q) was accepted", tc.from, tc.to, tc.since)
		}
	}
}

// An inverted window is refused: To before From silently returns nothing, which reads as "the estate was
// quiet" — the single most dangerous wrong answer this tool can give.
func TestAnInvertedWindowIsRefused(t *testing.T) {
	_, err := window("2026-08-06T05:00:00Z", "2026-08-06T02:00:00Z", "")
	if err == nil {
		t.Fatal("an inverted window was accepted — it returns no rows, which an operator reads as 'nothing " +
			"happened' rather than as 'you asked backwards'")
	}
}

// The caveat list must name lanes, and must say "none" rather than print an empty string — a blank where
// a warning belongs reads as no warning.
func TestTheTruncationCaveatSaysNoneRatherThanNothing(t *testing.T) {
	if got := truncatedList(nil); got != "none" {
		t.Errorf("truncatedList(nil) = %q, want %q — a blank where a caveat belongs reads as no caveat", got, "none")
	}
	if got := truncatedList([]string{"agent_step", "ingest_alert"}); got != "agent_step, ingest_alert" {
		t.Errorf("truncatedList = %q", got)
	}
}
