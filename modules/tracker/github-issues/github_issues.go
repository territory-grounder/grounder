// Package githubissues is the loadable GitHub Issues tracker module (spec/008 REQ-805).
//
// It implements adapters/tracker.Tracker: the four-verb ticketing contract (Open the trigger, Read,
// TransitionState, Comment the terminal audit sink), correlated by the issue number passed in as the id
// (INV-05). The session lifecycle never learns which backend it is; it maps the same contract that
// YouTrack/Jira/ServiceNow map, behind the same interface. The HTTP transport is injectable (a Doer) so
// the oracle drives the real code path against a fake backend without a live API. The API token is a
// secret reference (env:/file:), resolved per request, never a literal (INV-13).
//
// GitHub has no native "in progress" issue state, so this module folds GitHub's open|closed onto the
// tracker enum (closed -> Resolved, open -> Open) and never claims resolution for an unrecognized state.
//
// Provenance: [O] INV-05/INV-13/INV-18, spec/008.
package githubissues

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
const SourceType = "github-issues"

// Doer is the minimal HTTP contract the module depends on; *http.Client satisfies it, and tests inject a
// fake so the real request-building path runs against canned responses.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Module is the GitHub Issues tracker adapter, scoped to a single owner/repo. Construct with New.
type Module struct {
	baseURL  string
	owner    string
	repo     string
	tokenRef config.SecretRef
	http     Doer
}

// Option configures a Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP transport (a fake in tests, *http.Client in production).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// New builds a GitHub Issues tracker module for a base URL (e.g. "https://api.github.com"), the target
// owner and repo, and a token secret reference (e.g. "env:GITHUB_TOKEN"). The token is resolved per
// request and never held as a literal.
func New(baseURL, owner, repo string, tokenRef config.SecretRef, opts ...Option) *Module {
	m := &Module{
		baseURL:  strings.TrimRight(baseURL, "/"),
		owner:    owner,
		repo:     repo,
		tokenRef: tokenRef,
		http:     http.DefaultClient,
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

// do issues an authenticated JSON request against the GitHub REST API. The bearer token is resolved from
// its secret reference at call time (INV-13); a non-2xx response is an error.
func (m *Module) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	token, err := m.tokenRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("github-issues: resolve token: %w", err)
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
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
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
		return nil, fmt.Errorf("github-issues: %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// issuePath builds the per-issue REST path for the configured owner/repo. The id is the GitHub issue
// number as a string and is correlated exactly as passed (INV-05).
func (m *Module) issuePath(id string) string {
	return fmt.Sprintf("/repos/%s/%s/issues/%s", m.owner, m.repo, id)
}

// ghIssue is the subset of a GitHub issue this module reads.
type ghIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
}

// fromGitHubState folds GitHub's open|closed issue state onto the tracker-agnostic enum. GitHub has no
// in-progress state; an unrecognized state maps to Open (the least-progressed state) rather than silently
// claiming resolution.
func fromGitHubState(state string) tracker.State {
	switch state {
	case "closed":
		return tracker.StateResolved
	case "open":
		return tracker.StateOpen
	default:
		return tracker.StateOpen
	}
}

// toGitHubState maps the tracker-agnostic enum onto the GitHub state applied by TransitionState. GitHub
// has only open|closed, so Resolved closes the issue and everything else (Open, InProgress) reopens it.
func toGitHubState(s tracker.State) string {
	if s == tracker.StateResolved {
		return "closed"
	}
	return "open"
}

// Open opens and reads the entry issue — the triage trigger — returning it as the correlation anchor.
func (m *Module) Open(ctx context.Context, id string) (tracker.Issue, error) { return m.read(ctx, id) }

// Read returns the current issue state by correlation key (the issue number).
func (m *Module) Read(ctx context.Context, id string) (tracker.Issue, error) { return m.read(ctx, id) }

func (m *Module) read(ctx context.Context, id string) (tracker.Issue, error) {
	if id == "" {
		return tracker.Issue{}, fmt.Errorf("github-issues: empty issue id")
	}
	body, err := m.do(ctx, http.MethodGet, m.issuePath(id), nil)
	if err != nil {
		return tracker.Issue{}, err
	}
	var raw ghIssue
	if err := json.Unmarshal(body, &raw); err != nil {
		return tracker.Issue{}, fmt.Errorf("github-issues: malformed issue response: %w", err)
	}
	return tracker.Issue{ID: id, Title: raw.Title, State: fromGitHubState(raw.State)}, nil
}

// stateUpdate is the PATCH body that sets a GitHub issue's open|closed state.
type stateUpdate struct {
	State string `json:"state"`
}

// TransitionState moves the issue to a new state through the authenticated GitHub contract, keyed by the
// issue number (INV-05).
func (m *Module) TransitionState(ctx context.Context, id string, to tracker.State) error {
	if id == "" {
		return fmt.Errorf("github-issues: empty issue id")
	}
	_, err := m.do(ctx, http.MethodPatch, m.issuePath(id), stateUpdate{State: toGitHubState(to)})
	return err
}

// commentBody is the POST body for a GitHub issue comment.
type commentBody struct {
	Body string `json:"body"`
}

// Comment posts a body as the session sink (the terminal audit comment), keyed by the issue number.
func (m *Module) Comment(ctx context.Context, id, body string) error {
	if id == "" {
		return fmt.Errorf("github-issues: empty issue id")
	}
	_, err := m.do(ctx, http.MethodPost, m.issuePath(id)+"/comments", commentBody{Body: body})
	return err
}
