package observeprobe

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/tools/faultinjector"
)

// t0 is an arbitrary injection instant; the window is 10 minutes.
var (
	t0     = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	window = 10 * time.Minute
	tEnd   = t0.Add(window)
)

func ranRun() ProbeRun {
	return ProbeRun{Host: "gp-01", Class: faultinjector.ClassDeviceDown, InjectedAt: t0, WindowEnd: tEnd, Ran: true}
}

// A probe that ran and drew an alert INSIDE the window is OBSERVABLE — the fault was seen, the census
// "unobservable" reading was a false negative, reclassify.
func TestDecide_RanWithInWindowAlert_Observable(t *testing.T) {
	v, reason := Decide(ranRun(), []time.Time{t0.Add(2 * time.Minute)}, tEnd.Add(time.Second))
	if v != VerdictObservable {
		t.Fatalf("verdict = %q, want observable — an in-window alert is the fault being seen (%s)", v, reason)
	}
}

// THE FALSE-OBSERVABLE TRAP (RED-proves the core bug). A probe whose injected fault produced NO alert must
// NEVER read observable. Two shapes:
//   (a) no alert at all, window closed  → unobservable_confirmed, never observable
//   (b) an alert exists but OUTSIDE the window → must not count, so never observable
// A verdict that concluded "observable" from the probe having merely RUN (ignoring whether an alert actually
// surfaced in the window) would pass (a) as observable and (b) as observable — this test fails that bug.
func TestDecide_RanButNoInWindowAlert_IsNeverObservable(t *testing.T) {
	// (a) ran, window fully elapsed, zero alerts.
	if v, reason := Decide(ranRun(), nil, tEnd.Add(time.Minute)); v == VerdictObservable {
		t.Fatalf("verdict = observable for a ran probe with NO alert — the false-observable trap (%s)", reason)
	} else if v != VerdictUnobservableConfirmed {
		t.Fatalf("verdict = %q, want unobservable_confirmed for a ran probe, closed window, no alert", v)
	}

	// (b) an alert exists but BEFORE the injection and one AFTER the window — neither is the probe surfacing.
	outOfWindow := []time.Time{t0.Add(-time.Minute), tEnd.Add(time.Minute)}
	if v, reason := Decide(ranRun(), outOfWindow, tEnd.Add(time.Minute)); v == VerdictObservable {
		t.Fatalf("verdict = observable from an OUT-OF-WINDOW alert — the window filter is not being applied (%s)", reason)
	} else if v != VerdictUnobservableConfirmed {
		t.Fatalf("verdict = %q, want unobservable_confirmed — only in-window alerts may reclassify", v)
	}
}

// Boundary alerts (exactly at the injection instant and exactly at the window close) DO count — the window is
// inclusive of both ends, so a detector that fires the moment the fault lands is credited.
func TestDecide_BoundaryAlertsCount(t *testing.T) {
	for name, at := range map[string]time.Time{"at-injection": t0, "at-window-end": tEnd} {
		if v, _ := Decide(ranRun(), []time.Time{at}, tEnd.Add(time.Second)); v != VerdictObservable {
			t.Fatalf("%s alert: verdict = %q, want observable (window is inclusive of both ends)", name, v)
		}
	}
}

// A ran probe, window fully elapsed, no alert → a CONFIRMED coverage gap.
func TestDecide_RanClosedWindowNoAlert_UnobservableConfirmed(t *testing.T) {
	v, reason := Decide(ranRun(), nil, tEnd.Add(time.Second))
	if v != VerdictUnobservableConfirmed {
		t.Fatalf("verdict = %q, want unobservable_confirmed (%s)", v, reason)
	}
	if !v.Terminal() {
		t.Fatal("unobservable_confirmed must be terminal")
	}
}

// THE INCONCLUSIVE-vs-UNOBSERVABLE DISCRIMINATOR. A probe that did NOT run produced no observation, so its
// silence proves nothing — it must read INCONCLUSIVE, never unobservable_confirmed, EVEN with zero alerts and a
// long-closed window. A verdict that read "no alert" uniformly (without checking Ran) would call this a
// confirmed gap; this test fails that bug — the census must not manufacture blind spots from probes that never
// perturbed the estate.
func TestDecide_DidNotRun_IsInconclusiveNeverUnobservable(t *testing.T) {
	run := ranRun()
	run.Ran = false // the injection aborted before any effect
	v, reason := Decide(run, nil, tEnd.Add(time.Hour))
	if v == VerdictUnobservableConfirmed {
		t.Fatalf("verdict = unobservable_confirmed for a probe that NEVER RAN — a never-ran probe is not a coverage gap (%s)", reason)
	}
	if v != VerdictInconclusive {
		t.Fatalf("verdict = %q, want inconclusive for a probe that did not run", v)
	}
	if v.Terminal() {
		t.Fatal("inconclusive must NOT be terminal — the host stays re-probeable")
	}
}

// An explicitly aborted probe (kill switch / owner) is inconclusive too, for the same reason.
func TestDecide_Aborted_IsInconclusive(t *testing.T) {
	run := ranRun()
	run.Aborted = true
	if v, _ := Decide(run, []time.Time{t0.Add(time.Minute)}, tEnd.Add(time.Hour)); v != VerdictInconclusive {
		t.Fatalf("verdict = %q, want inconclusive for an aborted probe (no complete observation was made)", v)
	}
}

// THE CROSS-CYCLE GUARD. The SAME ran probe with NO in-window alert yields two DIFFERENT results depending on
// whether the window has closed — and the defect only shows across the boundary:
//   decided WHILE the window is open (same cycle it was injected) → INCONCLUSIVE (absence is not yet evidence)
//   decided AFTER the window closes (a later cycle)               → UNOBSERVABLE_CONFIRMED
// A one-shot verdict that skipped the `now < WindowEnd` guard would brand the just-injected probe a confirmed
// gap in cycle 1. This asserts the two cycles disagree, which is the whole point of not deciding too early.
func TestDecide_CrossCycle_WindowOpenIsInconclusive_ClosedIsConfirmed(t *testing.T) {
	run := ranRun()

	// Cycle 1: now is still inside the window, no alert yet.
	early, _ := Decide(run, nil, t0.Add(3*time.Minute))
	if early != VerdictInconclusive {
		t.Fatalf("window-open verdict = %q, want inconclusive — a probe injected this cycle must not be called a gap before its window closes", early)
	}

	// Cycle 2 (later): the window has closed, still no alert.
	late, _ := Decide(run, nil, tEnd.Add(time.Second))
	if late != VerdictUnobservableConfirmed {
		t.Fatalf("window-closed verdict = %q, want unobservable_confirmed", late)
	}

	if early == late {
		t.Fatal("cross-cycle defect: the same run decided the SAME before and after the window closed — the window boundary is not being honoured")
	}
}

// If an alert DID surface in the window, the verdict is observable regardless of when it is decided — a
// late-decided probe with an in-window alert is still observable, not a stale gap.
func TestDecide_WindowOpenWithAlert_ObservableImmediately(t *testing.T) {
	if v, _ := Decide(ranRun(), []time.Time{t0.Add(time.Minute)}, t0.Add(2*time.Minute)); v != VerdictObservable {
		t.Fatalf("verdict = %q, want observable — an in-window alert reclassifies as soon as it is seen", v)
	}
}
