// Package servicenow is the loadable ServiceNow tracker module (spec/008 REQ-805) for the connector
// fleet.
//
// It implements adapters/tracker.Tracker: the four-verb ticketing contract (Open the trigger, Read,
// TransitionState, Comment the terminal audit sink), correlated by the incident sys_id (INV-05). The
// session lifecycle never learns which backend it is; ServiceNow maps the same contract as YouTrack
// behind the same interface via the ServiceNow Table API. The HTTP transport is injectable (a Doer) so
// the oracle drives the real code path against a fake backend without a live API. The API token is a
// secret reference (env:/file:), resolved per request, never a literal (INV-13).
//
// Provenance: [O] INV-05/INV-13/INV-18, spec/008.
package servicenow

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
const SourceType = "servicenow"

// Doer is the minimal HTTP contract the module depends on; *http.Client satisfies it, and tests inject a
// fake so the real request-building path runs against canned responses.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// incidentStates is a deployment's ServiceNow `incident.state` numeric-code scheme for the three tracker
// states. `incident.state` is a per-instance configurable choice list — the 2/6/1 values are only the
// out-of-box ITSM defaults, so an instance that customized its state model needs its own codes here.
type incidentStates struct{ inProgress, resolved, open string }

// Module is the ServiceNow tracker adapter. Construct with New.
type Module struct {
	baseURL  string
	username string // ServiceNow instance username; the username half of HTTP Basic auth.
	tokenRef config.SecretRef
	http     Doer
	states   incidentStates // the deployment's incident.state code scheme (defaults to the ITSM out-of-box codes)
}

// Option configures a Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP transport (a fake in tests, *http.Client in production).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// WithStates overrides the ServiceNow `incident.state` numeric codes for the three tracker states — the
// deployment declares its own choice-list codes (config-not-code) rather than inheriting the out-of-box
// 2/6/1 that a customized instance almost certainly does not use. PATCHing a code the instance's state model
// lacks silently mis-sets or fails every close-out. An empty code keeps the default for that one state.
func WithStates(inProgress, resolved, open string) Option {
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

// New builds a ServiceNow tracker module for an instance base URL (e.g. "https://dev12345.service-now.com"),
// the instance username, and a password secret reference (e.g. "env:SERVICENOW_TOKEN"). ServiceNow Table
// API auth is HTTP Basic base64(username:password), so the username is the Basic username half and the
// secret — resolved per request, never held as a literal (INV-13) — is the password half.
func New(baseURL, username string, tokenRef config.SecretRef, opts ...Option) *Module {
	m := &Module{
		baseURL:  strings.TrimRight(baseURL, "/"),
		username: username,
		tokenRef: tokenRef,
		http:     http.DefaultClient,
		states:   incidentStates{inProgress: "2", resolved: "6", open: "1"},
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

// do issues an authenticated JSON request against the ServiceNow Table API. ServiceNow Table API auth is
// HTTP Basic base64(username:password) — a bare Bearer 401s on every request — so the password is resolved
// from its secret reference at call time (INV-13) and set as the Basic password; a non-2xx response is an error.
func (m *Module) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	token, err := m.tokenRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("servicenow: resolve token: %w", err)
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
	req.SetBasicAuth(m.username, token) // ServiceNow Table API auth = HTTP Basic base64(username:password).
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
		return nil, fmt.Errorf("servicenow: %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// incidentEnvelope is the subset of a ServiceNow Table API record this module reads. The Table API wraps
// a single record under a "result" object.
type incidentEnvelope struct {
	Result incidentRecord `json:"result"`
}

// incidentRecord is the incident fields this module reads: the short description (title) and the numeric
// state code (a string, e.g. "1", "2", "6").
type incidentRecord struct {
	ShortDescription string `json:"short_description"`
	State            string `json:"state"`
}

// fromServiceNowState folds a ServiceNow numeric incident state code into the tracker-agnostic enum, using
// the deployment's configured codes. ServiceNow's "Closed" (7) always folds to Resolved (closed ⊇ resolved
// for the 3-state model). An unrecognized code maps to Open (the least-progressed state) rather than
// silently claiming resolution.
func (m *Module) fromServiceNowState(code string) tracker.State {
	switch code {
	case m.states.inProgress:
		return tracker.StateInProgress
	case m.states.resolved, "7":
		return tracker.StateResolved
	default:
		// the configured open code and any unrecognized code fold to Open.
		return tracker.StateOpen
	}
}

// toServiceNowState maps the tracker-agnostic enum onto the deployment's ServiceNow numeric state code
// applied by TransitionState (the configured scheme, or the out-of-box ITSM defaults).
func (m *Module) toServiceNowState(s tracker.State) string {
	switch s {
	case tracker.StateInProgress:
		return m.states.inProgress
	case tracker.StateResolved:
		return m.states.resolved
	default:
		return m.states.open
	}
}

// Open opens and reads the entry incident — the triage trigger — returning it as the correlation anchor.
func (m *Module) Open(ctx context.Context, id string) (tracker.Issue, error) { return m.read(ctx, id) }

// Read returns the current incident state by correlation key (the sys_id).
func (m *Module) Read(ctx context.Context, id string) (tracker.Issue, error) { return m.read(ctx, id) }

func (m *Module) read(ctx context.Context, id string) (tracker.Issue, error) {
	if id == "" {
		return tracker.Issue{}, fmt.Errorf("servicenow: empty issue id")
	}
	body, err := m.do(ctx, http.MethodGet, "/api/now/table/incident/"+id, nil)
	if err != nil {
		return tracker.Issue{}, err
	}
	var env incidentEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return tracker.Issue{}, fmt.Errorf("servicenow: malformed incident response: %w", err)
	}
	return tracker.Issue{
		ID:    id,
		Title: env.Result.ShortDescription,
		State: m.fromServiceNowState(env.Result.State),
	}, nil
}

// stateUpdate is the PATCH body that sets an incident's numeric state code.
type stateUpdate struct {
	State string `json:"state"`
}

// TransitionState moves the incident to a new state through the authenticated Table API, keyed by the
// sys_id (INV-05).
func (m *Module) TransitionState(ctx context.Context, id string, to tracker.State) error {
	if id == "" {
		return fmt.Errorf("servicenow: empty issue id")
	}
	_, err := m.do(ctx, http.MethodPatch, "/api/now/table/incident/"+id, stateUpdate{State: m.toServiceNowState(to)})
	return err
}

// commentUpdate is the PATCH body appending an audit entry. ServiceNow appends work_notes as the incident
// audit trail rather than overwriting it.
type commentUpdate struct {
	WorkNotes string `json:"work_notes"`
}

// Comment posts a body as the session sink (the terminal audit note), keyed by the sys_id.
func (m *Module) Comment(ctx context.Context, id, body string) error {
	if id == "" {
		return fmt.Errorf("servicenow: empty issue id")
	}
	_, err := m.do(ctx, http.MethodPatch, "/api/now/table/incident/"+id, commentUpdate{WorkNotes: body})
	return err
}
