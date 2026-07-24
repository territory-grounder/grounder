// Package youtrack is the loadable YouTrack tracker module (spec/008 REQ-804, T-008-4) and the family
// lead for the tracker surface.
//
// It implements adapters/tracker.Tracker: the four-verb ticketing contract (Open the trigger, Read,
// TransitionState, Comment the terminal audit sink), correlated by the issue id (INV-05). The session
// lifecycle never learns which backend it is; Jira/GitHub Issues/ServiceNow map the same contract behind
// the same interface. The HTTP transport is injectable (a Doer) so the oracle drives the real code path
// against a fake backend without a live API. The API token is a secret reference (env:/file:), resolved
// per request, never a literal (INV-13).
//
// Provenance: [O] INV-05/INV-13/INV-18, spec/008.
package youtrack

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
const SourceType = "youtrack"

// Doer is the minimal HTTP contract the module depends on; *http.Client satisfies it, and tests inject a
// fake so the real request-building path runs against canned responses.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// stateNames is a deployment's YouTrack State-field VALUE names for the three tracker states. A YouTrack
// project's State bundle is per-project configurable — the default bundle's terminal values are `Fixed` /
// `Verified` (there is NO `Resolved` value), so the write path must use the deployment's actual value names.
type stateNames struct{ inProgress, resolved, open string }

// Module is the YouTrack tracker adapter. Construct with New.
type Module struct {
	baseURL    string
	tokenRef   config.SecretRef
	http       Doer
	states     stateNames // the project's State-field value names (defaults to the reference names)
	stateField string     // the custom-field name that holds the workflow state (default "State")
}

// Option configures a Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP transport (a fake in tests, *http.Client in production).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// WithStateNames overrides the YouTrack State-field value names the write path sets (config-not-code) — the
// deployment declares its bundle's actual values rather than the reference names. Crucially, the default
// YouTrack bundle has no `Resolved` value (its terminal values are `Fixed`/`Verified`), so a real project
// must map the resolved state onto a value that EXISTS or every close-out no-ops. An empty name keeps the
// reference default for that one state.
func WithStateNames(inProgress, resolved, open string) Option {
	return func(m *Module) {
		if inProgress != "" {
			m.states.inProgress = inProgress
		}
		if resolved != "" {
			m.states.resolved = resolved
		}
		if open != "" {
			m.states.open = open
		}
	}
}

// WithStateFieldName overrides the custom-field name that holds the workflow state (default "State"), for a
// project that renamed it. Empty keeps the default.
func WithStateFieldName(name string) Option {
	return func(m *Module) {
		if name != "" {
			m.stateField = name
		}
	}
}

// New builds a YouTrack tracker module for a base URL and a token secret reference (e.g.
// "env:YOUTRACK_TOKEN"). The token is resolved per request and never held as a literal.
func New(baseURL string, tokenRef config.SecretRef, opts ...Option) *Module {
	m := &Module{
		baseURL:    strings.TrimRight(baseURL, "/"),
		tokenRef:   tokenRef,
		http:       http.DefaultClient,
		states:     stateNames{inProgress: "In Progress", resolved: "Resolved", open: "Open"},
		stateField: "State",
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

// do issues an authenticated JSON request against the YouTrack REST API. The bearer token is resolved
// from its secret reference at call time (INV-13); a non-2xx response is an error.
func (m *Module) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	token, err := m.tokenRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("youtrack: resolve token: %w", err)
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
		return nil, fmt.Errorf("youtrack: %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Open opens and reads the entry issue — the triage trigger — returning it as the correlation anchor.
func (m *Module) Open(ctx context.Context, id string) (tracker.Issue, error) { return m.read(ctx, id) }

// Read returns the current issue state by correlation key (the issue id).
func (m *Module) Read(ctx context.Context, id string) (tracker.Issue, error) { return m.read(ctx, id) }

func (m *Module) read(ctx context.Context, id string) (tracker.Issue, error) {
	if id == "" {
		return tracker.Issue{}, fmt.Errorf("youtrack: empty issue id")
	}
	body, err := m.do(ctx, http.MethodGet, "/api/issues/"+id+"?fields=idReadable,summary,customFields(name,value(name))", nil)
	if err != nil {
		return tracker.Issue{}, err
	}
	var raw ytIssue
	if err := json.Unmarshal(body, &raw); err != nil {
		return tracker.Issue{}, fmt.Errorf("youtrack: malformed issue response: %w", err)
	}
	return tracker.Issue{ID: id, Title: raw.Summary, State: m.stateOf(raw)}, nil
}
