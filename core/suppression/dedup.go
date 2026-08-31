package suppression

import (
	"context"
	"errors"
	"time"
)

// TriageEntry is a prior triage-log entry the dedup stage compares against.
type TriageEntry struct {
	Host      string
	AlertRule string
	LoggedAt  time.Time
	// Suppressed is true if this prior entry was itself SUPPRESSED (not escalated into an incident). A
	// suppressed prior is not a valid dedup anchor — you dedup a re-fire against a still-open INCIDENT, not
	// against another silenced alert. Zero value (false = escalated) keeps a bare entry a valid anchor.
	Suppressed bool
	// IssueRef is the parent incident this entry escalated into, if any. A re-fire is deduped against it only
	// while that incident is CONFIRMED still open (see OpenIssue); an entry that names a parent whose openness
	// cannot be confirmed is not a valid dedup anchor — the re-fire escalates rather than being silenced.
	IssueRef string
}

// ErrMalformedEntry is returned when a prior entry is future-dated or negative-age relative to now — it
// is rejected at the envelope boundary rather than trusted as a duplicate (REQ-408, INV-04).
var ErrMalformedEntry = errors.New("suppression: triage entry is future-dated / negative-age — rejected at the boundary")

// recencySubWindow is a SHORT sub-window of the dedup Window, consulted ONLY on the unconfirmable-openness
// path (TG-459). When a re-fire matches a tracked parent (IssueRef set) but the parent's open/resolved state
// cannot be confirmed — no OpenIssue checker is wired, or the checker could not read the incident — a re-fire
// arriving while the prior anchor is still THIS fresh is far more likely a rapid duplicate of a STILL-open
// incident than a post-resolve re-alert, so it is deduped on pure recency instead of escalating; this
// restores the recency-dedup TG-354 gave up in the unconfirmable path (else EVERY unconfirmable re-fire
// escalates, risking an alert storm in multi/dark-tracker configs).
//
// Value: 5m, aligned with the burst-window scale (core/ingest.burstWindow = 5m) and kept STRICTLY inside the
// deployed dedup Window (10m in production, "dedup 10m0s"). A rapid duplicate re-fires within one monitoring
// scrape / alert-group interval — well under 5m — while a post-resolve re-fire only appears after the resolve
// transaction commits and the next scrape notices it, minutes-plus later and past this sub-window, so it
// still escalates as TG-354 intends. Shorter than the Window by construction, so the [recencySubWindow,
// Window] band stays a live "did it resolve by now?" escalation region rather than collapsing dedup back to
// pure recency.
const recencySubWindow = 5 * time.Minute

// DedupStage collapses a repeat of the same (host, alert_rule) within a recent window — but only against a
// still-open prior incident.
type DedupStage struct {
	Recent []TriageEntry
	Window time.Duration
	// OpenIssue reports whether a parent incident is CONFIRMED still open. Injected (a tracker lookup in
	// production, a fake in the oracle). Its contract is deliberately asymmetric: it returns true ONLY for a
	// positively-confirmed-open incident, and false for every other answer — closed, not found, or a read it
	// could not resolve. Deduping a re-fire against a tracked parent (IssueRef set) REQUIRES that true; when
	// this checker is nil (no entry tracker can resolve TG's external_ref) or returns false, openness is
	// UNCONFIRMED and the re-fire ESCALATES rather than being silenced as "a duplicate of an open incident"
	// nothing established — the safe direction for a detection gate is to surface, not to drop (TG-354). A
	// re-fire against an entry that names NO parent incident (IssueRef == "") is still window-recency deduped;
	// there is no incident identity whose openness to confirm.
	OpenIssue func(issueRef string) bool
	// EvalAt is the DEDUP evaluation clock — the wall-clock instant at which THIS worker is triaging, which is
	// the SAME clock the recent-triage log stamps its entries' LoggedAt with. It deliberately overrides the
	// `now` passed to Evaluate, because that `now` is the alert's OBSERVATION time (what freeze/scheduled match
	// on — when the alert FIRED), and the dedup question is different: "have WE recently triaged this same
	// (host, rule)?" — measured on triage time, not fire time. Feeding the observation clock here was TG-377:
	// a storm of out-of-order / ingestion-lagged alerts each read the others' anchors as future-dated (negative
	// age) and dedup suppressed 0 of 171. Zero EvalAt keeps the passed `now` (static-chain callers and oracles
	// that triage on a single clock).
	EvalAt time.Time
}

// Name implements Stage.
func (s *DedupStage) Name() Phase { return PhaseDedup }

// AcceptEntry reports whether a prior entry is a valid dedup candidate. A future-dated / negative-age
// entry is rejected at the boundary (ErrMalformedEntry, REQ-408). A well-formed entry is a candidate
// only while it falls inside [now-window, now).
func (s *DedupStage) AcceptEntry(e TriageEntry, now time.Time) (bool, error) {
	age := now.Sub(e.LoggedAt)
	if age < 0 { // logged AFTER now — future-dated / clock skew / negative age
		return false, ErrMalformedEntry
	}
	if age > s.Window {
		return false, nil // outside the dedup window — not a candidate, but well-formed
	}
	return true, nil
}

// Evaluate suppresses the alert as a duplicate if a well-formed prior entry for the same
// (host, alert_rule) lies within the window. A malformed prior entry fails OPEN (REQ-408): the alert
// escalates rather than being treated as a duplicate.
func (s *DedupStage) Evaluate(_ context.Context, a Alert, now time.Time) (Decision, error) {
	// Measure the window on the EVALUATION clock (when this worker triaged), not the alert's observation time —
	// see EvalAt. The recent-triage log stamps LoggedAt with this same clock, so age = EvalAt - LoggedAt is a
	// true elapsed-triage interval, immune to out-of-order / lagged alert arrival (TG-377).
	if !s.EvalAt.IsZero() {
		now = s.EvalAt
	}
	// An empty host is NOT a matchable dedup key (TG-389). The (host, alert_rule) match below treats
	// host=="" as equal to every other host=="", so an alert whose normalizer could not resolve a machine
	// would be suppressed as a "duplicate" of an unrelated hostless alert with the same rule — collapsing
	// seven distinct CiliumAgentNotReady nodes onto one key was the measured case. A hostless alert cannot
	// prove it is a re-fire of a specific incident, so it always escalates, never silently suppresses.
	if a.Host == "" {
		return escalate(a.ExternalRef, PhaseDedup, "host unresolved — not a matchable dedup key"), nil
	}
	for _, e := range s.Recent {
		if e.Host != a.Host || e.AlertRule != a.AlertRule {
			continue
		}
		if e.Suppressed {
			continue // a suppressed prior is not an incident to dedup against
		}
		ok, err := s.AcceptEntry(e, now)
		if err != nil {
			// a malformed entry must not be trusted as a duplicate — fail open to escalation.
			return escalate(a.ExternalRef, PhaseDedup, "malformed prior triage entry — fail open"), nil
		}
		if !ok {
			continue // outside the window — not a duplicate
		}
		// A prior that escalated into a PARENT INCIDENT (IssueRef set) may only silence a re-fire as "a
		// duplicate of an OPEN incident" when that incident is CONFIRMED still open. Suppression must be BACKED
		// by a positive open-confirmation — never by the mere ABSENCE of a closed-confirmation.
		if e.IssueRef != "" {
			if s.OpenIssue != nil {
				// A checker IS wired for this incident's identity. A POSITIVE open-confirmation ⇒ the re-fire is
				// a duplicate of that open incident ⇒ suppress. Anything else — the checker says RESOLVED, or it
				// cannot resolve TG's own external_ref namespace (both surface as a non-open answer) — is NOT a
				// positive open-confirmation, so we must NOT assert an openness nothing established: the re-fire
				// ESCALATES (TG-354, fail toward surfacing). Recency is IRRELEVANT here — a checker that can be
				// consulted has answered and did NOT confirm open; suppressing a fresh re-fire of a JUST-resolved
				// incident is exactly the silent-drop TG-354 exists to prevent.
				if s.OpenIssue(e.IssueRef) {
					return Decision{Outcome: OutcomeSuppressed, Phase: PhaseDedup, Reason: "duplicate of a confirmed-open incident within window", ExternalRef: a.ExternalRef}, nil
				}
				continue // checker present but not confirming open ⇒ escalate (TG-354)
			}
			// TG-459: NO checker is wired at all (OpenIssue == nil — the entry-tracker seam is dark: TG looks a
			// ticket up by its own external_ref, which the estate's tracker ids never contain). Openness is
			// genuinely UNKNOWABLE, so the absence of a close-confirmation is not evidence of resolution. Only
			// HERE — never when a checker exists and answered non-open — does a re-fire while the anchor is still
			// VERY FRESH (within the short recency sub-window) warrant recency-suppression: it is far more likely
			// a rapid duplicate of a plausibly-still-open incident than a post-resolve re-fire, and suppressing
			// it avoids a storm. This restores the recency-dedup TG-354 gave up in the no-tracker path. Freshness
			// is on the SAME eval clock the window used (now == EvalAt, normalized at the top of Evaluate);
			// AcceptEntry already proved this age non-negative and within Window. Once the anchor is old enough
			// that "did it resolve by now?" is a live question (past the sub-window), it ESCALATES (TG-354).
			if now.Sub(e.LoggedAt) <= recencySubWindow {
				return Decision{Outcome: OutcomeSuppressed, Phase: PhaseDedup, Reason: "duplicate within short recency window (no tracker to confirm openness)", ExternalRef: a.ExternalRef}, nil
			}
			continue // stale + no checker ⇒ escalate (TG-354)
		}
		// No parent incident was tracked (IssueRef == ""): fall back to pure window-recency dedup — a repeat of
		// the same (host, rule) inside the window, with no incident identity whose openness to confirm.
		return Decision{Outcome: OutcomeSuppressed, Phase: PhaseDedup, Reason: "duplicate within window (no parent incident tracked)", ExternalRef: a.ExternalRef}, nil
	}
	return escalate(a.ExternalRef, PhaseDedup, "no dedup match"), nil
}
