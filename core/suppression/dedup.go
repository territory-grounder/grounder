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
	// IssueRef is the parent incident this entry escalated into, if any. When a DedupStage.OpenIssue checker
	// is wired, a re-fire is deduped only while that incident is still OPEN.
	IssueRef string
}

// ErrMalformedEntry is returned when a prior entry is future-dated or negative-age relative to now — it
// is rejected at the envelope boundary rather than trusted as a duplicate (REQ-408, INV-04).
var ErrMalformedEntry = errors.New("suppression: triage entry is future-dated / negative-age — rejected at the boundary")

// DedupStage collapses a repeat of the same (host, alert_rule) within a recent window — but only against a
// still-open prior incident.
type DedupStage struct {
	Recent []TriageEntry
	Window time.Duration
	// OpenIssue reports whether a parent incident is still open. Injected (a tracker lookup in production,
	// a fake in the oracle). When nil the open-issue gate is skipped (window-only dedup); when set, a re-fire
	// is deduped only while the prior incident is open — a re-fire AFTER the prior closed is a NEW incident
	// and escalates, never silently suppressed.
	OpenIssue func(issueRef string) bool
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
		if s.OpenIssue != nil && e.IssueRef != "" && !s.OpenIssue(e.IssueRef) {
			continue // the prior incident closed — this is a genuine re-fire, not a duplicate; escalate
		}
		return Decision{Outcome: OutcomeSuppressed, Phase: PhaseDedup, Reason: "duplicate of an open incident within window", ExternalRef: a.ExternalRef}, nil
	}
	return escalate(a.ExternalRef, PhaseDedup, "no dedup match"), nil
}
