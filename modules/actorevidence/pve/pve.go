// Package pve is the Proxmox VE actor-evidence reader (spec/023 REQ-2306, the FIRST domain reader): it
// answers "WHO changed this guest's lifecycle state?" from the PVE task log, the authoritative audit
// trail for guest start/stop. It is READ-ONLY by construction, native-REST (no subprocess — the worker
// is distroless), and authenticates with a least-privilege READ-ONLY PVE token resolved as a
// core/config.SecretRef (INV-13) — NEVER the platform's `tg-actuate` write token.
//
// The smoking-gun it produces: a guest that stopped shows `root@pam` vzstop (an admin — coordinate),
// `root@pam!tg-actuate` vzstart (TG itself — self/no-op), nobody (an organic crash — unattributable), or
// an unknown principal (suspicious). Evidence collection is ADVISORY and fails OPEN (REQ-2307): a read
// error or timeout degrades the session to the pre-feature ladder, never blocks it.
package pve

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

// lifecycleTaskTypes are the PVE task verbs that CHANGE a guest's lifecycle state — the fault-shaped
// changes attribution reasons over (a vzstop/vzstart/qmstop/qmstart, an LXC/VM shutdown).
var lifecycleTaskTypes = map[string]bool{
	"vzstart": true, "vzstop": true, "vzshutdown": true, "vzreboot": true,
	"qmstart": true, "qmstop": true, "qmshutdown": true, "qmreboot": true,
}

// Doer is the minimal HTTP contract; *http.Client satisfies it and tests inject a fake PVE cluster.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Module is the PVE actor-evidence Reader.
type Module struct {
	base     string
	tokenRef config.SecretRef
	http     Doer
	timeout  time.Duration
}

// Option configures the Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP transport (a fake in tests, an *http.Client in production).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// WithTimeout bounds each Read (config, with a compiled ceiling) so a hung PVE endpoint cannot stall triage.
func WithTimeout(d time.Duration) Option {
	return func(m *Module) {
		if d > 0 && d <= 15*time.Second {
			m.timeout = d
		}
	}
}

// New returns the PVE actor-evidence reader for a PVE base URL and a READ-ONLY token SecretRef.
func New(baseURL string, tokenRef config.SecretRef, opts ...Option) *Module {
	m := &Module{base: strings.TrimRight(baseURL, "/"), tokenRef: tokenRef, http: http.DefaultClient, timeout: 8 * time.Second}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Domain identifies this reader's evidence family (keys the sanctioned-principal and self-identity config).
func (m *Module) Domain() string { return "pve" }

// ReadOnly is always true — the seam is read-only by construction.
func (m *Module) ReadOnly() bool { return true }

var _ actorevidence.Reader = (*Module)(nil)

// Read returns the lifecycle actor-evidence records for a guest NAME within [since, until]: it resolves
// the name to node/vmid via GET /cluster/resources (the same fail-closed resolution the actuation module
// uses), reads GET /nodes/{node}/tasks, and keeps the lifecycle rows in the window. Covered=true — the
// PVE task log is the authoritative audit trail for guest lifecycle (REQ-2304 half 2). An unresolvable
// guest or a read error is advisory: the caller treats this domain's evidence as absent (REQ-2307).
func (m *Module) Read(ctx context.Context, target string, since, until time.Time) ([]attribution.Evidence, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	node, vmid, err := m.resolveGuest(ctx, target)
	if err != nil {
		return nil, err
	}
	tasks, err := m.listTasks(ctx, node, vmid, since)
	if err != nil {
		return nil, err
	}
	guestID := strconv.Itoa(vmid)
	var out []attribution.Evidence
	for _, t := range tasks {
		// The node /tasks endpoint names the guest in the "id" field (a string, e.g. "101101011"), NOT a
		// "vmid" field — the reader keyed on a field the API never returns, which silently dropped every row.
		if t.ID != guestID || !lifecycleTaskTypes[t.Type] {
			continue
		}
		at := time.Unix(t.StartTime, 0).UTC()
		if at.Before(since) || at.After(until) {
			continue
		}
		out = append(out, attribution.Evidence{
			Domain: m.Domain(),
			// The task log records the base user in "user" (e.g. "root@pam") and the API-token id, when the
			// action used a token, in a SEPARATE "tokenid" field. TG's own heals authenticate with a token, so
			// the self-identity ("root@pam!tg-actuate") only exists once the two are recombined — "user" alone is
			// the bare principal and would never match the self actor.
			Actor:      principal(t.User, t.TokenID),
			ActionKind: t.Type,
			Target:     target,
			ObservedAt: at,
			Ref:        t.UPID,
			Covered:    true,
		})
	}
	return out, nil
}

// principal recombines the PVE task log's split identity into the canonical "user@realm!tokenid" form the
// attributor matches against (a bare "user@realm" when the action used no token).
func principal(user, tokenID string) string {
	if tokenID == "" {
		return user
	}
	return user + "!" + tokenID
}

// resourcesRow is one row of GET /cluster/resources?type=vm.
type resourcesRow struct {
	Node   string `json:"node"`
	Name   string `json:"name"`
	Vmid   int    `json:"vmid"`
	Status string `json:"status"`
}

// resolveGuest resolves a guest NAME to its node + vmid, FAIL CLOSED on zero or multiple matches.
func (m *Module) resolveGuest(ctx context.Context, name string) (string, int, error) {
	body, err := m.get(ctx, "/api2/json/cluster/resources?type=vm")
	if err != nil {
		return "", 0, err
	}
	var env struct {
		Data []resourcesRow `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", 0, fmt.Errorf("pve: cluster resources unparseable: %w", err)
	}
	node, vmid, found := "", 0, false
	for _, r := range env.Data {
		if r.Name != name {
			continue
		}
		if found {
			return "", 0, fmt.Errorf("pve: guest name %q is ambiguous across the cluster (fail closed)", name)
		}
		node, vmid, found = r.Node, r.Vmid, true
	}
	if !found {
		return "", 0, fmt.Errorf("pve: guest %q not found (fail closed)", name)
	}
	return node, vmid, nil
}

// taskRow is one row of GET /nodes/{node}/tasks. The guest is named in "id" (a string), and a token-authed
// action carries its token id in "tokenid" — the base user stays in "user".
type taskRow struct {
	UPID      string `json:"upid"`
	User      string `json:"user"`
	TokenID   string `json:"tokenid"`
	Type      string `json:"type"`
	ID        string `json:"id"`
	StartTime int64  `json:"starttime"`
	Status    string `json:"status"`
}

// listTasks reads the node's task log, BOUNDED to the guest and the lookback window: without a `since`
// bound (and a raised `limit`) the endpoint returns only the newest ~50 rows node-wide, which on a busy
// node can exclude the very lifecycle change under investigation. `source=all` includes in-flight tasks.
func (m *Module) listTasks(ctx context.Context, node string, vmid int, since time.Time) ([]taskRow, error) {
	q := url.Values{}
	q.Set("source", "all")
	q.Set("limit", "2000")
	if vmid > 0 {
		q.Set("vmid", strconv.Itoa(vmid))
	}
	if !since.IsZero() {
		q.Set("since", strconv.FormatInt(since.Unix(), 10))
	}
	body, err := m.get(ctx, fmt.Sprintf("/api2/json/nodes/%s/tasks?%s", node, q.Encode()))
	if err != nil {
		return nil, err
	}
	var env struct {
		Data []taskRow `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("pve: task log unparseable: %w", err)
	}
	return env.Data, nil
}

func (m *Module) get(ctx context.Context, path string) ([]byte, error) {
	return m.do(ctx, http.MethodGet, path)
}

func (m *Module) do(ctx context.Context, method, path string) ([]byte, error) {
	token, err := m.tokenRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("pve: read-only token unresolvable (INV-13): %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+token)
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		// Bound the echoed body without slicing past its end: PVE 401/403 bodies are short (often empty), and
		// an unconditional [:200] panicked on any trimmed body under 200 bytes — turning a benign auth failure
		// (which REQ-2307 must degrade to "evidence absent") into a crash.
		msg := strings.TrimSpace(string(b))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("pve: %s %s → %d: %s", method, path, resp.StatusCode, msg)
	}
	return b, nil
}
