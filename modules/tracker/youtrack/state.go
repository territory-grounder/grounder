package youtrack

import (
	"context"
	"net/http"

	tracker "github.com/territory-grounder/grounder/adapters/tracker"
)

// ytIssue is the subset of a YouTrack issue this module reads.
type ytIssue struct {
	IDReadable   string          `json:"idReadable"`
	Summary      string          `json:"summary"`
	CustomFields []ytCustomField `json:"customFields"`
}

// ytCustomField is one YouTrack custom field; for the State field, value.name is the workflow state.
type ytCustomField struct {
	Name  string `json:"name"`
	Value struct {
		Name string `json:"name"`
	} `json:"value"`
}

// stateOf extracts the tracker-agnostic State from the issue's configured State custom field (m.stateField).
func (m *Module) stateOf(i ytIssue) tracker.State {
	for _, f := range i.CustomFields {
		if f.Name == m.stateField {
			return m.fromYouTrackState(f.Value.Name)
		}
	}
	return tracker.StateOpen
}

// fromYouTrackState folds a YouTrack State value name into the tracker-agnostic enum. It recognizes the
// deployment's configured value names AND the default-bundle TERMINAL names — the resolved states
// (`Fixed`/`Done`/`Closed`/`Verified`) AND the dismissed states (`Cancelled`/`Duplicate`) — all of which
// fold to Resolved (the agnostic enum's only "closed" value), so reads stay correct whether or not the
// project renamed its bundle. Folding the dismissed states is load-bearing: the read is what the live dedup
// gate consults to decide whether a re-fire joins an OPEN anchor issue (suppress) or escalates a fresh
// incident. A Cancelled/Duplicate anchor is CLOSED, so a re-fire must escalate — the predecessor's
// `State: -Done,-Cancelled,-Duplicate` reuse filter. Treating them as Open silently suppressed genuine
// re-fires. An unrecognized name still maps to Open rather than silently claiming resolution.
func (m *Module) fromYouTrackState(name string) tracker.State {
	switch name {
	case m.states.inProgress:
		return tracker.StateInProgress
	case m.states.resolved, "Resolved", "Fixed", "Done", "Closed", "Verified", "Cancelled", "Duplicate":
		return tracker.StateResolved
	default:
		return tracker.StateOpen
	}
}

// toYouTrackState maps the tracker-agnostic enum onto the deployment's YouTrack State value name applied by
// TransitionState (the configured names, or the reference defaults).
func (m *Module) toYouTrackState(s tracker.State) string {
	switch s {
	case tracker.StateInProgress:
		return m.states.inProgress
	case tracker.StateResolved:
		return m.states.resolved
	default:
		return m.states.open
	}
}

// stateFieldUpdate is the POST body that sets an issue's State custom field.
type stateFieldUpdate struct {
	CustomFields []stateField `json:"customFields"`
}

type stateField struct {
	Name  string     `json:"name"`
	Type  string     `json:"$type"`
	Value stateValue `json:"value"`
}

type stateValue struct {
	Name string `json:"name"`
}

// TransitionState moves the issue to a new state through the authenticated YouTrack contract, keyed by the
// issue id (INV-05).
func (m *Module) TransitionState(ctx context.Context, id string, to tracker.State) error {
	body := stateFieldUpdate{CustomFields: []stateField{{
		Name:  m.stateField,
		Type:  "StateIssueCustomField",
		Value: stateValue{Name: m.toYouTrackState(to)},
	}}}
	_, err := m.do(ctx, http.MethodPost, "/api/issues/"+id, body)
	return err
}

// commentBody is the POST body for a YouTrack issue comment.
type commentBody struct {
	Text string `json:"text"`
}

// Comment posts a body as the session sink (the terminal audit comment), keyed by the issue id.
func (m *Module) Comment(ctx context.Context, id, body string) error {
	_, err := m.do(ctx, http.MethodPost, "/api/issues/"+id+"/comments", commentBody{Text: body})
	return err
}
