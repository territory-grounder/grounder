package main

// THE BOOT POSTURE MUST BE RE-PUBLISHED AFTER THE MODE AUTHORITY IS BOUND (TG-143).
//
// The worker publishes its live mutation posture on a ticker, and once immediately at boot. That boot call
// runs BEFORE BindMode, so it writes may_actuate=false regardless of the persisted mode. The console then
// showed a TRANSIENT Shadow for up to one ticker interval after every restart — and a restart happens on
// every merge, because the AWX deploy recreates the worker. For that window the console misread an
// ACTUATING estate as read-only.
//
// That is a display bug in the sense that runtime behaviour was always correct. It is not a harmless one:
// posture is the surface an operator consults to answer "is this thing allowed to change my estate right
// now", and an answer that is wrong for a bounded window after every deploy trains people to distrust the
// surface — or worse, to trust a stale "Shadow" and act as though nothing can move.
//
// The fix is a second publishPosture() after BindMode. It is one line, it is idempotent, and NOTHING
// asserted it — which in this repository is the shape that regresses. This guard reads the composition
// root and pins the ORDER, the same technique core/seal/composition_root_test.go uses, because the
// property is genuinely about wiring order and there is no seam to unit-test it through: reproducing it
// live would mean booting a worker against Temporal and a populated mode store and racing a ticker.

import (
	"os"
	"strings"
	"testing"
)

func TestBootPostureIsRepublishedAfterBindMode(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read composition root: %v", err)
	}
	text := string(src)

	bind := strings.Index(text, "chokepoint.BindMode(")
	if bind < 0 {
		t.Fatal("cmd/worker/main.go no longer calls chokepoint.BindMode — this guard is reading a file that " +
			"no longer describes the boot sequence, and would otherwise pass by checking nothing")
	}
	// Every publishPosture() CALL after the bind — COMMENT LINES DO NOT COUNT.
	//
	// The first version of this guard scanned raw text and its own killing mutation exposed the flaw: the
	// explanatory comment above the fix contains the words "publishPosture() above ran BEFORE BindMode",
	// so deleting the real call left the guard GREEN. A check satisfied by prose ABOUT a control rather
	// than by the control is the failure mode this repository keeps rediscovering — it is the same defect
	// TG-326's guard had, found the same way, on the same day.
	var after int
	bindLine := strings.Count(text[:bind], "\n")
	for n, line := range strings.Split(text, "\n") {
		if n <= bindLine {
			continue
		}
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "//") {
			continue
		}
		if strings.Contains(code, "publishPosture()") {
			after++
		}
	}
	// Vacuity floor: if the ticker and the boot call were both removed, `after == 0` would be true for a
	// reason this test is not about, and the message below would send a reader hunting the wrong thing.
	if !strings.Contains(text, "publishPosture := func()") {
		t.Fatal("publishPosture is no longer defined in the worker composition root — posture publication " +
			"has been restructured, so re-derive this guard rather than deleting it")
	}
	if after == 0 {
		t.Error("no publishPosture() call appears AFTER chokepoint.BindMode(). The boot publish runs before " +
			"the mode authority is bound, so it writes may_actuate=false whatever the persisted mode is — " +
			"the console then shows a TRANSIENT Shadow for up to one ticker interval after every restart, " +
			"and a restart happens on every merge. An operator reading posture in that window sees an " +
			"actuating estate reported as read-only.")
	}
}
