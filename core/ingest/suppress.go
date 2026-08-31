package ingest

import "time"

// INCIDENT-SCOPED SUPPRESSION — repeats of a STILL-OPEN incident, not repeats within a time window.
//
// WHY NOT THE OBVIOUS DESIGN. Pipeline.Process already dedups, but it is batch-local and stateless, and the
// batches this estate actually delivers average 1.14 events (librenms 1,393 groups, biggest 3; alertmanager
// 39 groups, biggest 2). Flap needs 3 fires and burst needs 3 incidents, so neither can trip inside a real
// batch: giving that chain a production caller would satisfy an audit and suppress nothing.
//
// The duplication that exists is CROSS-batch. Excluding injected-fault windows, 7 days of live traffic held
// 400 alerts across 107 distinct (source, rule, host) keys — 293 repeats, a 73.3% repeat rate, worst key 25
// fires. So there is a real operator-facing noise problem, and it needs durable state.
//
// ★ AND THE OBVIOUS DURABLE DESIGN — "drop repeats of a key within 24h" — WOULD HAVE BROKEN THE BENCHMARK.
// A1 (detection recall) correlates each injected_fault against an ingest_alert for the same host inside a
// detection window. Dedup keeps the first occurrence, so a single fault survives. But the fault harness
// RE-INJECTS the same (host, rule) many times a day — measured: 34 fires of Service-up/down on one guest in
// 24h, and on the six noisiest hosts EVERY alert arrived inside an injected-fault window (70/70, 55/55,
// 54/54, 52/52, 45/45, 43/43). Under a 24h window the 2nd..Nth injection's FIRST alert is dropped as a
// duplicate of the previous injection's, those faults score as UNDETECTED, and A1 collapses — while the
// change reads as a pure noise win.
//
// So suppression is scoped to the INCIDENT, not the clock: a repeat is suppressed only while the incident is
// still OPEN, and a recovery RESETS the key. A re-injection after a recovery is a new incident and is always
// admitted. That is the property the oracle pins and the mutation control attacks.

// Fire is one prior observation of a dedup key, oldest-to-newest as stored.
type Fire struct {
	At        time.Time
	Recovered bool // true when this observation CLEARED the key (a recovery/up event) rather than raised it
}

// SuppressDecision is the verdict for a candidate alert, and the evidence for it. The reason is carried so a
// suppression can be audited after the fact — a decision that records only its own verdict cannot be
// reviewed, which is how 140 novelty polls became unanswerable.
type SuppressDecision struct {
	Suppress bool
	Reason   string
	// OpenSince is the first fire of the still-open incident this repeat belongs to. Zero when admitting.
	OpenSince time.Time
}

// MaxOpenIncident bounds how long an incident may stay "open" without any recovery before a new fire is
// treated as a fresh incident. Without it a key whose recovery is never delivered — a monitoring gap, not a
// quiet estate — would be suppressed forever, which converts a noise filter into a blind spot.
const MaxOpenIncident = 6 * time.Hour

// DecideSuppress reports whether a new fire at now is a repeat of a still-OPEN incident for the same key.
//
// history is that key's prior observations in ascending time order. The walk runs BACKWARDS and stops at the
// first thing it finds:
//   - a recovery  ⇒ the incident closed, this fire opens a new one ⇒ ADMIT
//   - a fire      ⇒ the incident is still open ⇒ SUPPRESS (unless it has gone stale, see MaxOpenIncident)
//   - nothing     ⇒ never seen ⇒ ADMIT
//
// Fail-open by construction: every path that is not a confirmed still-open repeat ADMITS. An alert wrongly
// admitted is noise; an alert wrongly suppressed is an incident nobody sees.
func DecideSuppress(history []Fire, now time.Time) SuppressDecision {
	for i := len(history) - 1; i >= 0; i-- {
		h := history[i]
		if h.Recovered {
			return SuppressDecision{Suppress: false, Reason: "incident closed by a recovery — this fire opens a new one"}
		}
		// The most recent observation is an un-recovered fire: the incident is open.
		if now.Sub(h.At) > MaxOpenIncident {
			return SuppressDecision{Suppress: false,
				Reason: "the open incident is stale (no recovery within the bound) — admitting rather than suppressing forever"}
		}
		// Walk back to the START of this open run so the operator can see when it began.
		openSince := h.At
		for j := i - 1; j >= 0 && !history[j].Recovered; j-- {
			openSince = history[j].At
		}
		return SuppressDecision{Suppress: true, OpenSince: openSince,
			Reason: "repeat of an incident that is still open (no recovery since it was raised)"}
	}
	return SuppressDecision{Suppress: false, Reason: "no prior observation of this key"}
}
