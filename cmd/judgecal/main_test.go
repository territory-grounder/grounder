package main

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/diagcorpus"
)

func at(min int) time.Time { return time.Date(2026, 7, 27, 12, min, 0, 0, time.UTC) }

// THE POPULATION FILTER IS THE WHOLE INSTRUMENT.
//
// The corpus's unit of truth is the FAULT; the judge scores a SESSION. Live, one stopped guest trips FOUR
// LibreNMS rules and so raises four sessions, of which TG need act on one. Scoring all four against the
// fault's truth is the error that made device-down read 73.7% instead of 89.7%. Taking the FIRST is no better
// — measured, it put "first session accuracy" at 82/202 against a true 82%. Only faults with exactly one
// session are commensurable, and these tests hold that line.

func TestOnlyFaultsWithExactlyOneSessionAreScored(t *testing.T) {
	items := []diagcorpus.Item{
		{FaultID: 1, ExternalRef: "solo", FaultType: "disk-fill", At: at(1)},
		{FaultID: 2, ExternalRef: "multi-a", FaultType: "device-down", At: at(2)},
		{FaultID: 2, ExternalRef: "multi-b", FaultType: "device-down", At: at(3)},
		{FaultID: 2, ExternalRef: "multi-c", FaultType: "device-down", At: at(4)},
		{FaultID: 3, ExternalRef: "solo2", FaultType: "device-down", At: at(5)},
	}
	got := soleSessionFaults(items)
	if len(got) != 2 {
		t.Fatalf("only the two single-session faults are commensurable, got %d: %+v", len(got), got)
	}
	for _, it := range got {
		if it.FaultID == 2 {
			t.Fatalf("a fault with sibling sessions must be excluded — its per-session truth is ambiguous")
		}
	}
	// MUTATION CONTROL: taking the FIRST session per fault instead would return 3 here and silently
	// reintroduce the very bias this filter exists to remove.
	if len(got) == 3 {
		t.Fatal("this is the first-session-per-fault result, not the sole-session one")
	}
}

// Order is stable and chronological, so a report is reproducible run to run.
func TestSoleSessionFaultsAreChronological(t *testing.T) {
	got := soleSessionFaults([]diagcorpus.Item{
		{FaultID: 9, ExternalRef: "late", FaultType: "disk-fill", At: at(30)},
		{FaultID: 8, ExternalRef: "early", FaultType: "disk-fill", At: at(10)},
	})
	if len(got) != 2 || got[0].ExternalRef != "early" {
		t.Fatalf("want chronological order, got %+v", got)
	}
}

// An empty corpus yields an empty population rather than a panic — the calibration then reports UNCALIBRATED,
// which is not the same as failing.
func TestEmptyCorpusYieldsEmptyPopulation(t *testing.T) {
	if got := soleSessionFaults(nil); len(got) != 0 {
		t.Fatalf("want empty, got %+v", got)
	}
}
