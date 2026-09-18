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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	readOnly   bool       // when set, every mutating verb refuses BEFORE issuing a request (see WithReadOnly)
}

// Option configures a Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP transport (a fake in tests, *http.Client in production).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// WithReadOnly makes the module STRUCTURALLY incapable of writing: every mutating verb — including the
// four-verb contract's TransitionState and Comment — refuses before a request leaves the process.
//
// It exists for a specific, non-hypothetical hazard. TG reads the shared YouTrack corpus to equalize
// incident memory against the predecessor, but the PREDECESSOR IS DRIVEN BY THOSE SAME ISSUES AND READS
// THEM. A single TG comment on a live incident is readable by the other arm mid-campaign, which
// contaminates the comparison in a way no later analysis can undo. "We will not call the write verbs" is
// an intention; this is a control — and the difference between the two is the whole reason this project
// is in recovery.
//
// It is also the right default for any read-only integration: the module can delete issues and comments,
// and a posture that cannot write cannot destroy a tracker through a bug.
func WithReadOnly() Option { return func(m *Module) { m.readOnly = true } }

// WithReadOnlyUnless is the composition-root form: pass the deployment's WRITES-ENABLED flag and get the
// read-only posture unless writes were explicitly turned on. Phrasing it as "unless" keeps the safe
// posture on the zero value, so a caller that forgets the flag cannot accidentally arm writes.
func WithReadOnlyUnless(writesEnabled bool) Option {
	return func(m *Module) { m.readOnly = !writesEnabled }
}

// ErrReadOnly is returned by every mutating verb when the module is configured read-only.
var ErrReadOnly = errors.New("youtrack: module is READ-ONLY — writes are refused by configuration")

// guardWrite is the single chokepoint every mutating verb passes through. One function, so a new write
// verb cannot be added that forgets the check without also skipping the obvious idiom.
func (m *Module) guardWrite() error {
	if m.readOnly {
		return ErrReadOnly
	}
	return nil
}

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

// compile-time proof the module satisfies the stable tracker interface — and the optional
// entry-creation capability (TG-490).
var (
	_ tracker.Tracker       = (*Module)(nil)
	_ tracker.EntryCreator  = (*Module)(nil)
	_ tracker.EntrySearcher = (*Module)(nil)
)

// SearchEntry implements adapters/tracker.EntrySearcher (TG-490 fix): a project-scoped text
// search for the incident key every entry-ticket body carries. A READ — no write guard. The
// query quotes the key so the tracker matches it as a phrase, and the project scope keeps a
// shared instance's other projects out of the answer.
func (m *Module) SearchEntry(ctx context.Context, project, incidentKey string) ([]tracker.Issue, error) {
	if strings.TrimSpace(project) == "" || strings.TrimSpace(incidentKey) == "" {
		return nil, fmt.Errorf("youtrack: search entry: project and incident key are required")
	}
	// `sort by: created desc` is EXPLICIT (round-2 finding #1): the adopt-arm picks found[0] as
	// "the newest", and YouTrack's default ordering for /api/issues is not a contract — an
	// unordered result could bind a reservation to the OLDER of two duplicates. YouTrack's text
	// search covers summary AND description (the incident key lives in the description); the
	// api_test drill pins the query shape, and the live e2e at arming verifies the index answers.
	q := fmt.Sprintf("project: %s %q sort by: created desc", project, incidentKey)
	body, err := m.do(ctx, http.MethodGet, "/api/issues?fields=idReadable,summary&$top=5&query="+url.QueryEscape(q), nil)
	if err != nil {
		return nil, fmt.Errorf("youtrack: search entry in %s: %w", project, err)
	}
	var raw []struct {
		IDReadable string `json:"idReadable"`
		Summary    string `json:"summary"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("youtrack: malformed search response: %w", err)
	}
	out := make([]tracker.Issue, 0, len(raw))
	for _, r := range raw {
		if strings.TrimSpace(r.IDReadable) == "" {
			continue
		}
		out = append(out, tracker.Issue{ID: r.IDReadable, Title: r.Summary})
	}
	return out, nil
}

// CreateEntry implements adapters/tracker.EntryCreator (TG-490): files TG's own entry ticket for
// an alert-sourced incident through the SAME authenticated Create the console write path uses.
// The returned Issue carries the READABLE id (e.g. TGOPS-123) as the correlation key — the id the
// four-verb contract's Read/Comment/TransitionState resolve, and the id humans grep logs for.
func (m *Module) CreateEntry(ctx context.Context, project, summary, description string) (tracker.Issue, error) {
	if strings.TrimSpace(project) == "" || strings.TrimSpace(summary) == "" {
		return tracker.Issue{}, fmt.Errorf("youtrack: create entry: project and summary are required")
	}
	rich, err := m.Create(ctx, NewIssue{Project: project, Summary: summary, Description: description})
	if err != nil {
		return tracker.Issue{}, fmt.Errorf("youtrack: create entry in %s: %w", project, err)
	}
	id := rich.Readable
	if strings.TrimSpace(id) == "" {
		id = rich.ID
	}
	if strings.TrimSpace(id) == "" {
		return tracker.Issue{}, fmt.Errorf("youtrack: create entry in %s: backend returned no issue id", project)
	}
	return tracker.Issue{ID: id, Title: rich.Summary}, nil
}

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
