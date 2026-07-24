// Package netbox is the NetBox changelog actor-evidence reader (spec/023 T-023-10, REQ-2306/REQ-2307, the
// CMDB domain). Given a target device it resolves the device id (/api/dcim/devices/?name=<target>) and
// reads that object's change history (/api/core/object-changes/?changed_object_id=<id>) within the window,
// emitting ONE minimized Evidence per change naming the target device, the changing user as the actor, and
// the changelog action (create/update/delete) as the action. READ-ONLY and FAIL-CLOSED: a missing/ambiguous
// device, an unresolvable token, or a non-2xx aborts with an error.
//
// v4.5.5 workaround (grounded live 2026-07-24): the DEFAULT object-changes representation 500s on this
// NetBox (a server-side ObjectChangeSerializer bug in the nested prechange/postchange field). The actor
// fields are retrieved intact with a SPARSE fieldset — ?fields=id,time,action,user,user_name — which never
// serializes the broken nested field. The changelog moved to /api/core/ in v4.x (the old /api/extras/ 404s).
//
// Provenance: [O] spec/023 REQ-2306/2307, T-023-10.
package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/adapters/actorevidence"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/config"
)

// Doer is the minimal HTTP surface (so a test drives a fake without a live NetBox).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Module is the read-only NetBox changelog reader.
type Module struct {
	base     string
	tokenRef config.SecretRef
	http     Doer
	timeout  time.Duration
	// pageSize is the DRF page size; maxPages bounds the paginated walk so a device with an enormous in-window
	// change history fails closed (explicit truncation error) rather than silently under-reporting.
	pageSize int
	maxPages int
}

// Option configures a Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP client (a fake in tests).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// WithTimeout caps the per-Read deadline (bounded to a sane ceiling).
func WithTimeout(d time.Duration) Option {
	return func(m *Module) {
		if d > 0 && d <= 20*time.Second {
			m.timeout = d
		}
	}
}

// New builds a NetBox reader against baseURL, authenticating with the token at tokenRef (NetBox uses the
// "Token <value>" scheme). The token is resolved at use time, never stored (INV-13).
func New(baseURL string, tokenRef config.SecretRef, opts ...Option) *Module {
	m := &Module{base: strings.TrimRight(baseURL, "/"), tokenRef: tokenRef, http: http.DefaultClient, timeout: 12 * time.Second, pageSize: 200, maxPages: 25}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Domain implements actorevidence.Reader.
func (m *Module) Domain() string { return "netbox" }

// ReadOnly implements actorevidence.Reader — this reader never mutates NetBox.
func (m *Module) ReadOnly() bool { return true }

var _ actorevidence.Reader = (*Module)(nil)

// Read returns one Evidence per changelog entry on the target device within [since, until].
func (m *Module) Read(ctx context.Context, target string, since, until time.Time) ([]attribution.Evidence, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	deviceID, err := m.resolveDevice(ctx, target)
	if err != nil {
		return nil, err
	}
	changes, err := m.changes(ctx, deviceID, since, until)
	if err != nil {
		return nil, err
	}
	var out []attribution.Evidence
	for _, c := range changes {
		at := c.observedAt()
		if at.IsZero() || at.Before(since) || at.After(until) {
			continue
		}
		actor := c.actor()
		if actor == "" {
			continue // an actor-evidence record with no principal is not admissible (REQ-2306/2307)
		}
		out = append(out, attribution.Evidence{
			Domain:     m.Domain(),
			Actor:      actor,
			ActionKind: c.actionKind(),
			Target:     target,
			ObservedAt: at,
			Ref:        strconv.Itoa(c.ID),
			Covered:    true,
		})
	}
	return out, nil
}

// ---- NetBox API shapes (only the fields we read) ---------------------------------------------------

type deviceRow struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func (m *Module) resolveDevice(ctx context.Context, name string) (int, error) {
	q := url.Values{}
	q.Set("name", name)
	q.Set("fields", "id,name")
	q.Set("limit", "2")
	body, err := m.get(ctx, "/api/dcim/devices/?"+q.Encode())
	if err != nil {
		return 0, err
	}
	var env struct {
		Results []deviceRow `json:"results"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return 0, fmt.Errorf("netbox: devices response unparseable: %w", err)
	}
	match := 0
	for _, d := range env.Results {
		if d.Name == name {
			if match != 0 {
				return 0, fmt.Errorf("netbox: device name %q is ambiguous (fail closed)", name)
			}
			match = d.ID
		}
	}
	if match == 0 {
		return 0, fmt.Errorf("netbox: device %q not found (fail closed)", name)
	}
	return match, nil
}

type changeRow struct {
	ID       int    `json:"id"`
	Time     string `json:"time"`
	UserName string `json:"user_name"`
	User     struct {
		Username string `json:"username"`
	} `json:"user"`
	Action struct {
		Value string `json:"value"`
	} `json:"action"`
}

func (c changeRow) observedAt() time.Time {
	if c.Time == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, c.Time); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

func (c changeRow) actor() string {
	if u := strings.TrimSpace(c.User.Username); u != "" {
		return u
	}
	return strings.TrimSpace(c.UserName)
}

func (c changeRow) actionKind() string {
	if c.Action.Value != "" {
		return c.Action.Value
	}
	return "change"
}

func (m *Module) changes(ctx context.Context, deviceID int, since, until time.Time) ([]changeRow, error) {
	q := url.Values{}
	q.Set("changed_object_id", strconv.Itoa(deviceID))
	// SPARSE fieldset — omits the broken nested prechange/postchange field (v4.5.5 500 workaround).
	q.Set("fields", "id,time,action,user,user_name")
	q.Set("ordering", "-time")
	q.Set("limit", strconv.Itoa(m.pageSize))
	if !since.IsZero() {
		q.Set("time_after", since.UTC().Format(time.RFC3339))
	}
	if !until.IsZero() {
		q.Set("time_before", until.UTC().Format(time.RFC3339))
	}
	// Follow DRF pagination so a device with many in-window changes is not silently truncated at one page.
	// The server already bounds by time_after/time_before, so every page is in-window; we just gather them.
	next := "/api/core/object-changes/?" + q.Encode()
	var all []changeRow
	for pages := 0; next != ""; pages++ {
		if pages >= m.maxPages {
			return nil, fmt.Errorf("netbox: device %d has more than %d pages of in-window changes — coverage would truncate (fail closed, not silent)", deviceID, m.maxPages)
		}
		body, err := m.get(ctx, next)
		if err != nil {
			return nil, err
		}
		var env struct {
			Next    string      `json:"next"`
			Results []changeRow `json:"results"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, fmt.Errorf("netbox: object-changes unparseable: %w", err)
		}
		all = append(all, env.Results...)
		next = pathOf(env.Next)
	}
	return all, nil
}

// pathOf converts a DRF `next` URL (absolute) to the path+query this client's get() expects; blank ends the walk.
func pathOf(next string) string {
	if next == "" {
		return ""
	}
	u, err := url.Parse(next)
	if err != nil {
		return ""
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}

// ---- transport -------------------------------------------------------------------------------------

func (m *Module) get(ctx context.Context, path string) ([]byte, error) {
	token, err := m.tokenRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("netbox: read-only token unresolvable (INV-13): %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+token)
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("netbox: GET %s → %d: %s", path, resp.StatusCode, msg)
	}
	return b, nil
}
