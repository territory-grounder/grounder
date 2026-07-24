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
