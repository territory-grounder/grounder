// Package observeprobe is the fault-injection PROBE arm of the observation census (TG-180 part 2), and the
// census's own NULL TEST. The census (core/observe) classifies a silent estate entity "unobservable" from a
// never-fired-ever proxy — a hypothesis that TG cannot see it. That hypothesis is only FALSIFIABLE if injecting
// a real fault into the entity is checked for whether it surfaces: census = hypothesis, probe = test. Without
// this arm the census would produce a confident coverage number that cannot be wrong, which is exactly the
// circularity the ticket exists to avoid.
//
// This package is the MACHINERY, built DEFAULT-OFF. Planning (which guinea-pig to probe, with what fault) and
// the verdict (did it surface?) are PURE and exhaustively unit-tested; the orchestrator that would actually
// perturb the estate is gated behind an arming flag that is absent by default, so nothing here injects a fault
// until an owner arms it (the epic's lowest safety sub-score — it deliberately breaks production). The real
// injection reuses tools/faultinjector (its pool agreement, no-stacking ledger, name-assert, and self-reverting
// restore); this package never re-implements injection.
package observeprobe

import (
	"time"

	"github.com/territory-grounder/grounder/tools/faultinjector"
)

// Verdict is the closed observability outcome of one probe.
type Verdict string

const (
	// VerdictPending — the probe was injected and its observation window has not closed yet. Not terminal; it
	// is decided on a LATER cycle once the window elapses (the cross-cycle discipline).
	VerdictPending Verdict = "pending"
	// VerdictObservable — an alert surfaced on the probed host INSIDE the window: the injected fault was seen,
	// so the entity IS observable and its census "unobservable" reading was a false negative (never-tested, not
	// blind). The orchestrator reclassifies it and raises a finding.
	VerdictObservable Verdict = "observable"
	// VerdictUnobservableConfirmed — the probe RAN, the window fully elapsed, and NO alert surfaced: a real,
	// TESTED coverage gap (not mere silence). This is the only reading that turns the census's suspicion into
	// proof — and the scorecard dimension counts it.
	VerdictUnobservableConfirmed Verdict = "unobservable_confirmed"
	// VerdictInconclusive — no conclusion may be drawn: either the probe never actually perturbed the estate
	// (aborted before any effect / kill-switched), or its window is still open. This is DELIBERATELY DISTINCT
	// from unobservable-confirmed: "no alert because nothing was injected" must never be read as "no alert
	// because TG is blind" — conflating them would manufacture coverage gaps out of probes that never ran.
	VerdictInconclusive Verdict = "inconclusive"
)

// Terminal reports whether a verdict is a final answer. A non-terminal verdict (pending / inconclusive) is
// re-examined or re-probed rather than counted as coverage: pending awaits its window, and inconclusive means
// the question is still open because the probe never made a complete observation.
func (v Verdict) Terminal() bool {
	return v == VerdictObservable || v == VerdictUnobservableConfirmed
}

// ProbeRun is the durable record of one probe: a fault of Class was (or was not) injected on Host at
// InjectedAt, and its observation window closes at WindowEnd.
type ProbeRun struct {
	Host       string
	Class      faultinjector.Class
	InjectedAt time.Time
	WindowEnd  time.Time
	// Ran is whether the injected fault ACTUALLY committed on the estate. False ⇒ the probe aborted before any
	// effect (a refused precondition, a blind-snapshot skip) — the estate was never perturbed, so no alert is
	// expected and its absence proves nothing. This is the load-bearing discriminator: without it a probe that
	// never ran would read exactly like a structural blind spot.
	Ran bool
	// Aborted is whether the probe was explicitly stopped mid-flight (kill switch / owner). Like !Ran it forces
	// INCONCLUSIVE — an aborted probe made no complete observation over its window.
	Aborted bool
}

// Decide is the PURE verdict function. Given a probe run, the times of alerts observed ON THE PROBED HOST, and
// the current time, it returns the observability verdict and a human-readable reason.
//
// alertTimes must already be scoped to run.Host (the alert reader does that); Decide applies the WINDOW filter
// itself, so an alert OUTSIDE [InjectedAt, WindowEnd] can never be mistaken for the probe surfacing. That is
// the false-observable trap closed inside the pure function: an "observable" verdict requires a real alert
// that both belongs to this host AND fell inside the window the fault was live.
func Decide(run ProbeRun, alertTimes []time.Time, now time.Time) (Verdict, string) {
	// (1) A probe that did not run — or was aborted, or carries no injection time — made NO observation. This
	// is INCONCLUSIVE and must NEVER be confused with a confirmed gap: nothing was injected, so nothing was
	// expected to alert, so the entity's observability is exactly as unknown as before the probe.
	if !run.Ran || run.Aborted || run.InjectedAt.IsZero() {
		return VerdictInconclusive, "probe did not run (aborted before any effect / kill-switched) — no observation was made; the entity's status is unchanged, this is NOT a confirmed coverage gap"
	}

	// (2) Did any alert surface on this host INSIDE the window? An alert at or after the injection and no later
	// than the window close is the fault being SEEN — the entity is observable. An alert outside the window is
	// unrelated to this probe and deliberately does not count.
	for _, t := range alertTimes {
		if !t.Before(run.InjectedAt) && !t.After(run.WindowEnd) {
			return VerdictObservable, "an alert surfaced on the probed host within the observation window — the injected fault WAS seen; the entity is observable and was silent only because untested"
		}
	}

	// (3) The probe ran and no in-window alert surfaced — BUT if the window is still open, absence of an alert
	// is not yet evidence of blindness (the alert may still arrive). This is the CROSS-CYCLE guard: concluding
	// here, in the same cycle the probe was injected, would brand every just-injected probe an unobservable gap
	// before its fault had any chance to be detected.
	if now.Before(run.WindowEnd) {
		return VerdictInconclusive, "observation window still open — the probe ran but no alert has surfaced YET; absence is not yet evidence of blindness (re-decide after the window closes)"
	}

	// (4) The probe ran, the window fully elapsed, and no alert ever surfaced: a real, TESTED coverage gap.
	return VerdictUnobservableConfirmed, "the observation window has fully elapsed and the injected fault surfaced NO alert — a confirmed coverage gap: TG is structurally blind to this entity"
}
