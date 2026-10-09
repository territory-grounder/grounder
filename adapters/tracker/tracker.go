// Package tracker is the stable interface for the tracker surface: a ticketing system that is both the
// triage entry trigger and the terminal audit sink, correlated by issue id.
//
// Provenance: [O] INV-05 (the issue id is the correlation key across the session lifecycle), INV-18 (one
// implementation per source type), spec/008. YouTrack is the day-1 backend; Jira, GitHub Issues, and
// ServiceNow map the same four-verb contract onto their own APIs behind THIS interface, so a tracker
// change is a configuration selection and not a code fork — the session lifecycle never learns which
// backend it is.
package tracker

import "context"

// State is a tracker-agnostic issue state the session lifecycle transitions through. Each backend maps
// these onto its own state model.
type State string

const (
	StateOpen       State = "open"
	StateInProgress State = "in_progress"
	StateResolved   State = "resolved"
)

// Issue is the tracker-agnostic view of a ticket used as the correlation anchor.
type Issue struct {
	ID    string // the correlation key across the session lifecycle (INV-05)
	Title string
	State State
}

// EntryCreator is the OPTIONAL write-half capability a backend may add to the four-verb contract
// (TG-490): filing TG's OWN entry ticket for an alert-sourced incident. Deliberately not part of
// Tracker — a read-lane backend stays a four-verb citizen, and the worker asserts this capability
// at wiring time (absent ⇒ the creator stays dark, loudly). The inputs are pure DATA rendered
// from the ingest record (INV-08: no model token reaches this effect path).
type EntryCreator interface {
	// CreateEntry files one entry ticket in the given project and returns it as the correlation
	// anchor. Creation against a remote tracker is AT-LEAST-ONCE by nature — no backend offers an
	// idempotency key — so the CALLER owns the discipline: reserve durably BEFORE creating (the
	// reservation removes the incident from every future work list), complete the reservation
	// after, and settle a crashed attempt by SEARCHING for the incident key the ticket body
	// carries (EntrySearcher) — adopt what exists, create only on a provable none. The
	// entryfile package implements exactly that; call this verb outside it with care.
	CreateEntry(ctx context.Context, project, summary, description string) (Issue, error)
}

// EntrySearcher is the resolver's half of the two-phase filing (TG-490 fix): find the entry
// ticket(s) already carrying an incident key in the given project — the adopt-vs-create question
// a stale reservation asks after a crash between create and complete. Optional exactly like
// EntryCreator; a backend without it leaves stale reservations to the resolver's create arm
// (at-least-once, loudly).
type EntrySearcher interface {
	// SearchEntry returns the issues in project whose text carries the incident key, newest
	// first. An empty slice means "provably none found" (ok to create); an error means the
	// question could not be answered (do NOT create — try again next pass).
	SearchEntry(ctx context.Context, project, incidentKey string) ([]Issue, error)
}

// Tracker is the four-verb ticketing contract every backend satisfies: Open (the trigger), Read,
// TransitionState, and Comment (the terminal audit sink).
type Tracker interface {
	// SourceType is the source/vendor slug (e.g. "youtrack", "jira", "github-issues", "servicenow").
	SourceType() string
	// Open opens and reads the entry issue — the triage trigger — returning it as the correlation anchor.
	Open(ctx context.Context, id string) (Issue, error)
	// Read returns the current issue state by correlation key.
	Read(ctx context.Context, id string) (Issue, error)
	// TransitionState moves the issue to a new state through the backend's authenticated contract.
	TransitionState(ctx context.Context, id string, to State) error
	// Comment posts a body as the session sink (e.g. the terminal audit comment).
	Comment(ctx context.Context, id, body string) error
}
