package tracker

import (
	"context"
	"time"
)

// HISTORY — the OPTIONAL capability that lets TG read the incident record that predates it.
//
// A tracker at an established site holds years of incidents with human-written resolutions: how the
// engineers already working there solved this exact fault, in their words, on their machines. That is
// the single richest source of estate-specific knowledge available to TG on its first day, and the only
// one that exists BEFORE TG has done anything.
//
// WHY IT IS SEPARATE FROM Tracker. The four-verb contract (Open/Read/TransitionState/Comment) is what
// the session lifecycle requires of EVERY backend — adding a fifth verb would make every backend
// implement search before any of them could be used at all. History is a capability a backend MAY have;
// callers detect it with a type assertion and degrade honestly when it is absent. That keeps INV-18's
// one-implementation-per-source-type intact without holding the feature hostage to the least capable
// backend.
//
// WHY IT WAS WRITTEN. Until 2026-08-01 the get-tracker-history agent tool was gated behind a type
// assertion on the concrete *youtrack.Module. Every other configured tracker — ServiceNow, Jira, GitHub
// Issues, all implementing the same four verbs — fell through to the else arm and logged "no tracker
// configured", which was FALSE: a tracker was configured, it simply was not that one. A ServiceNow site
// therefore ran TG blind on its own weeks of session history while a decade of its own incident record
// sat one API call away.
//
// EVERYTHING RETURNED HERE IS UNTRUSTED DATA (INV-08). Tracker text is written by humans and, at some
// sites, by another autonomous system. It is an observation about what was done before, never an
// instruction about what to do now, and every consumer renders it quoted and inert.
type History interface {
	// SearchIncidents returns prior incidents matching a host and (optionally) an alert rule, newest
	// first, bounded by limit. The match is deliberately loose — tracker summaries are free text written
	// by people, and an exact-match search over human prose finds nothing.
	//
	// An empty result and an error are DIFFERENT facts and must not be conflated: "this site has no
	// record of this host failing this way" is a finding, while "the tracker could not be read" is an
	// outage. A backend that returned (nil, nil) on a failed read would teach the agent that the estate
	// has no history.
	SearchIncidents(ctx context.Context, host, rule string, limit int) ([]HistoricalIncident, error)
}

// HistoricalIncident is one prior incident from the shared record.
type HistoricalIncident struct {
	// ID is the tracker's human-readable id (e.g. "IFRNLLEI01PRD-2198", "INC0010023") — the reference an
	// engineer at this site would recognise and can look up.
	ID string
	// Summary is the issue title as filed.
	Summary string
	// State is the workflow state as the tracker reports it, in the TRACKER's own vocabulary
	// ("Resolved", "Closed", "6") rather than folded into tracker.State. A consumer showing prior history
	// to a human should show what the ticket says; folding it loses the site's own distinctions.
	State string
	// Filed is when the incident was opened. Zero means the backend did not report one — unknown, never
	// "now": a fabricated timestamp would make an ancient ticket rank as fresh precedent.
	Filed time.Time
	// Source is the vendor slug the incident came from ("youtrack", "servicenow"). Stamped when several
	// trackers are read together: ids live in per-vendor namespaces, and an unqualified "INC0010023"
	// beside a "IFRNLLEI01PRD-2198" tells a reader nothing about where to go look.
	Source string
	// Comments is the discussion, OLDEST FIRST, as written. At most sites the resolution is written in a
	// comment ("restarted the guest; the journal was the consumer"), not in a field — a history that
	// returned issues without comments would look like it worked and carry almost none of the value.
	Comments []string
}
