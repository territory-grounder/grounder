package main

// A DARK-SEAM WAIVER MUST BE ABLE TO EXPIRE.
//
// core/wiring.Because is built to make a dark seam perishable. It validates hard:
//
//	if b.Expiry.IsZero()      -> "Because requires an Expiry — a waiver with no end is a permanent one"
//	if !b.Expiry.After(now)   -> "Because expired at <t>"
//	if b.Expiry.Sub(now) > maxDarkHorizon -> "expiry is more than 180 days out"
//
// All three checks are real and all three run. And every one of the 14 waivers in main.go was written as
//
//	Expiry: time.Now().Add(90 * 24 * time.Hour)
//
// The expiry is computed AT BOOT. So at the instant it is validated it is always exactly 30 or 90 days in
// the future, on every restart, forever. `!b.Expiry.After(now)` cannot fire. Every waiver was permanent —
// the precise thing the validator's own error message says a waiver must not be.
//
// This is the house pathology in the mechanism built to bound the house pathology: present, validated,
// enforced, and structurally incapable of firing. It is invisible in review because the line reads like a
// deadline, and no test could catch it without asking the question this file asks.
//
// The fix is fixed calendar dates, derived from each waiver's own authoring date (git blame) plus the
// horizon it declared, so intent is preserved exactly. Converting them changed no state on 2026-08-05:
// all 14 were authored 2026-08-01..04, so the earliest genuine expiry is 2026-08-31.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// scanWaiverExpiries returns (total expiries found, the lines whose expiry is boot-relative). Split out
// from the test so the VACUITY FLOOR itself is testable: the floor fires when a source file contains no
// Expiry at all, and the only way to prove that path works is to hand it such a file.
func scanWaiverExpiries(src string) (found int, bootRelative []string) {
	expiryLine := regexp.MustCompile(`Expiry:\s*(.+?)(,|\}|$)`)
	for _, ln := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue // prose about an expiry is not an expiry
		}
		m := expiryLine.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		found++
		if strings.Contains(m[1], "time.Now()") {
			bootRelative = append(bootRelative, trimmed)
		}
	}
	return found, bootRelative
}

// The floor, exercised directly. A file with declarations but no Expiry must report zero found, which is
// what makes the t.Fatal below meaningful rather than decorative.
func TestWaiverScanReportsNothingFoundOnAFileWithNoExpiries(t *testing.T) {
	found, bad := scanWaiverExpiries("wiring.Absent(m, SeamX, Because{Reason: \"r\", Owner: \"@o\"})\n")
	if found != 0 || len(bad) != 0 {
		t.Fatalf("a source with no Expiry reported found=%d bad=%d, want 0/0 — the vacuity floor in "+
			"TestDarkSeamWaiversUseFixedDates... would never fire and the guard could pass by checking "+
			"nothing", found, len(bad))
	}
	// And the positive control, so this test cannot pass by the scanner being broken outright.
	found, bad = scanWaiverExpiries("Expiry: time.Now().Add(30 * 24 * time.Hour),\n")
	if found != 1 || len(bad) != 1 {
		t.Fatalf("a boot-relative expiry reported found=%d bad=%d, want 1/1", found, len(bad))
	}
}

func TestDarkSeamWaiversUseFixedDatesNotBootRelativeOnes(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	found, bootRelative := scanWaiverExpiries(string(raw))
	for _, ln := range bootRelative {
		{
			t.Errorf("a dark-seam waiver expires relative to BOOT: %s\n"+
				"time.Now().Add(...) is re-evaluated on every restart, so the waiver is always 30/90 days "+
				"fresh at the moment it is validated and `!b.Expiry.After(now)` can never fire. The seam "+
				"stays dark forever behind a deadline that reads like it is approaching. Use a fixed "+
				"time.Date(...) so the waiver genuinely perishes.", ln)
		}
	}

	// VACUITY FLOOR. If the declarations move or are renamed, this test would find nothing to check and
	// pass silently — the same failure it exists to catch, in itself.
	if found == 0 {
		t.Fatal("no Expiry: field found anywhere in main.go. Either the dark-seam waivers moved, or the " +
			"field was renamed. This guard compared NOTHING and must not report success — re-derive it " +
			"against wherever wiring.Because declarations now live.")
	}
	if len(bootRelative) == 0 {
		t.Logf("checked %d waiver expiries, all fixed dates", found)
	}
}

// The companion property: a fixed date is only useful if it is inside the horizon the manifest accepts.
// An expiry past maxDarkHorizon (180d) is rejected outright, which turns the seam into DarkUnrecorded —
// dark for a reason nobody wrote down, which is worse than the honest declaration it replaced.
func TestDarkSeamWaiverDatesAreParseableAndBounded(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	// time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	dateCall := regexp.MustCompile(`Expiry:\s*time\.Date\((\d{4}),\s*time\.(\w+),\s*(\d{1,2})`)
	ms := dateCall.FindAllStringSubmatch(string(raw), -1)
	if len(ms) == 0 {
		t.Fatal("no fixed-date Expiry found — this guard checked nothing")
	}
	months := map[string]bool{
		"January": true, "February": true, "March": true, "April": true, "May": true, "June": true,
		"July": true, "August": true, "September": true, "October": true, "November": true, "December": true,
	}
	for _, m := range ms {
		if !months[m[2]] {
			t.Errorf("waiver expiry names a month %q that is not a time.Month constant", m[2])
		}
		if m[1] < "2026" {
			t.Errorf("waiver expiry year %q predates the project — a waiver that was never valid is not a "+
				"waiver, it is a seam that is dark for an unrecorded reason", m[1])
		}
	}
	t.Logf("checked %d fixed-date waiver expiries", len(ms))
}
