// Package jira is the loadable Jira Cloud tracker module (spec/008 REQ-805) and a member of the tracker
// surface family led by the frozen YouTrack exemplar.
//
// It implements adapters/tracker.Tracker: the four-verb ticketing contract (Open the trigger, Read,
// TransitionState, Comment the terminal audit sink), correlated by the issue key (INV-05). The session
// lifecycle never learns which backend it is; the same contract sits behind the same interface across
// YouTrack/Jira/GitHub Issues/ServiceNow. The HTTP transport is injectable (a Doer) so the oracle drives
// the real code path against a fake backend without a live API. The API token is a secret reference
// (env:/file:), resolved per request, never a literal (INV-13).
//
// Provenance: [O] INV-05/INV-13/INV-18, spec/008.
package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	tracker "github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/core/config"
)

// SourceType is the vendor slug this module serves.
const SourceType = "jira"

// The reference-workflow Jira transition ids the module POSTs to move an issue between the three tracker
// states. A real deployment's Jira workflow rarely uses these exact ids, so they are only the DEFAULT —
// WithTransitions (config-not-code) declares the deployment's own scheme. POSTing a transition id that does
// not exist in the target workflow 404s, so a wrong hardcoded id would silently fail every close-out.
const (
	transitionInProgress = "21"
	transitionResolved   = "31"
	transitionOpen       = "11"
)

// transitions is a deployment's Jira workflow transition-id scheme for the three tracker states.
type transitions struct{ inProgress, resolved, open string }

// Doer is the minimal HTTP contract the module depends on; *http.Client satisfies it, and tests inject a
// fake so the real request-building path runs against canned responses.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Module is the Jira Cloud tracker adapter. Construct with New.
type Module struct {
	baseURL  string
	email    string // Jira account email; the username half of HTTP Basic API-token auth.
	tokenRef config.SecretRef
	http     Doer
	tr       transitions // the deployment's workflow transition-id scheme (defaults to the reference ids)
}

// Option configures a Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP transport (a fake in tests, *http.Client in production).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// WithTransitions overrides the Jira workflow transition ids POSTed to move an issue between the three
// tracker states — the deployment declares its own scheme (config-not-code) rather than inheriting the
// reference ids that its workflow almost certainly does not use. An empty id keeps the reference default for
// that one state, so an operator can override only the ids that differ.
func WithTransitions(inProgress, resolved, open string) Option {
	return func(m *Module) {
		if inProgress != "" {
			m.tr.inProgress = inProgress
		}
		if resolved != "" {
			m.tr.resolved = resolved
		}
		if open != "" {
			m.tr.open = open
		}
	}
}

// New builds a Jira tracker module for a base URL, the account email, and a token secret reference
// (e.g. "env:JIRA_TOKEN"). Jira Cloud API-token auth is HTTP Basic base64(email:api_token), so the
// email is the username half and the token — resolved per request, never held as a literal — is the
// password half.
func New(baseURL, email string, tokenRef config.SecretRef, opts ...Option) *Module {
	m := &Module{
		baseURL:  strings.TrimRight(baseURL, "/"),
		email:    email,
		tokenRef: tokenRef,
		http:     http.DefaultClient,
		tr:       transitions{inProgress: transitionInProgress, resolved: transitionResolved, open: transitionOpen},
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// SourceType implements adapters/tracker.Tracker.
func (m *Module) SourceType() string { return SourceType }

// compile-time proof the module satisfies the stable tracker interface.
var _ tracker.Tracker = (*Module)(nil)

// do issues an authenticated JSON request against the Jira REST API. Jira Cloud API-token auth is HTTP
// Basic base64(email:api_token) — a bare Bearer 401s on every request — so the token is resolved from
// its secret reference at call time (INV-13) and set as the Basic password; a non-2xx response is an error.
func (m *Module) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	token, err := m.tokenRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("jira: resolve token: %w", err)
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(m.email, token) // Jira Cloud API-token auth = HTTP Basic base64(email:api_token).
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jira: %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// jiraIssue is the subset of a Jira issue this module reads from GET .../issue/{key}?fields=summary,status.
type jiraIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary string `json:"summary"`
		Status  struct {
			Name string `json:"name"`
		} `json:"status"`
	} `json:"fields"`
}

// fromJiraStatus folds a Jira status name into the tracker-agnostic enum. An unrecognized status maps to
// Open (the least-progressed state) rather than silently claiming resolution.
func fromJiraStatus(name string) tracker.State {
	switch name {
	case "In Progress", "In Review":
		return tracker.StateInProgress
	case "Resolved", "Done", "Closed", "Fixed", "Verified":
		return tracker.StateResolved
	default:
		return tracker.StateOpen
	}
}

// toJiraTransition maps the tracker-agnostic enum onto the deployment's Jira transition id POSTed by
// TransitionState (the configured scheme, or the reference-workflow defaults).
func (m *Module) toJiraTransition(s tracker.State) string {
	switch s {
	case tracker.StateInProgress:
		return m.tr.inProgress
	case tracker.StateResolved:
		return m.tr.resolved
	default:
		return m.tr.open
	}
}

// Open opens and reads the entry issue — the triage trigger — returning it as the correlation anchor.
func (m *Module) Open(ctx context.Context, id string) (tracker.Issue, error) { return m.read(ctx, id) }

// Read returns the current issue state by correlation key (the issue key).
func (m *Module) Read(ctx context.Context, id string) (tracker.Issue, error) { return m.read(ctx, id) }

func (m *Module) read(ctx context.Context, id string) (tracker.Issue, error) {
	if id == "" {
		return tracker.Issue{}, fmt.Errorf("jira: empty issue id")
	}
	body, err := m.do(ctx, http.MethodGet, "/rest/api/2/issue/"+id+"?fields=summary,status", nil)
	if err != nil {
		return tracker.Issue{}, err
	}
	var raw jiraIssue
	if err := json.Unmarshal(body, &raw); err != nil {
		return tracker.Issue{}, fmt.Errorf("jira: malformed issue response: %w", err)
	}
	return tracker.Issue{ID: id, Title: raw.Fields.Summary, State: fromJiraStatus(raw.Fields.Status.Name)}, nil
}

// transitionRef is the "transition" object in the transitions POST body.
type transitionRef struct {
	ID string `json:"id"`
}

// transitionBody is the POST body for /rest/api/2/issue/{key}/transitions.
type transitionBody struct {
	Transition transitionRef `json:"transition"`
}

// TransitionState moves the issue to a new state through the authenticated Jira contract, keyed by the
// issue key (INV-05).
func (m *Module) TransitionState(ctx context.Context, id string, to tracker.State) error {
	if id == "" {
		return fmt.Errorf("jira: empty issue id")
	}
	body := transitionBody{Transition: transitionRef{ID: m.toJiraTransition(to)}}
	_, err := m.do(ctx, http.MethodPost, "/rest/api/2/issue/"+id+"/transitions", body)
	return err
}

// commentBody is the POST body for /rest/api/2/issue/{key}/comment.
type commentBody struct {
	Body string `json:"body"`
}

// Comment posts a body as the session sink (the terminal audit comment), keyed by the issue key.
func (m *Module) Comment(ctx context.Context, id, body string) error {
	if id == "" {
		return fmt.Errorf("jira: empty issue id")
	}
	_, err := m.do(ctx, http.MethodPost, "/rest/api/2/issue/"+id+"/comment", commentBody{Body: body})
	return err
}
