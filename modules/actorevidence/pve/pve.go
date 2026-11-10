// Package pve is the Proxmox VE actor-evidence reader (spec/023 REQ-2306, the FIRST domain reader): it
// answers "WHO changed this guest's lifecycle state?" from the PVE task log, the authoritative audit
// trail for guest start/stop. It is READ-ONLY by construction, native-REST (no subprocess — the worker
// is distroless), and authenticates with a least-privilege READ-ONLY PVE token resolved as a
// core/config.SecretRef (INV-13) — NEVER the platform's `tg-actuate` write token.
//
// The smoking-gun it produces: a guest that stopped shows `root@pam` vzstop (an admin — coordinate),
// `root@pam!tg-actuate` vzstart (TG itself — self/no-op), nobody (an organic crash — unattributable), or
// an unknown principal (suspicious). Evidence collection is ADVISORY (REQ-2307): a read error or timeout never
// fails the session. A TRANSIENT failure is retried briefly first (TG-652), and for a start-guest the attribute
// activity HOLDS on a read that still fails, rather than healing over a stop it could not see (TG-652).
package pve

import (
	"context"
	"encoding/json"
	"errors"
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
// changes attribution reasons over (a vzstop/vzstart/qmstop/qmstart, an LXC/VM shutdown) — and, since TG-694, every
// other task that can power a guest on or off: a suspend/resume, a restore (which may start the restored guest), a
// snapshot rollback (which may restore a running state) and a VM reset. The causality survey judges a start-guest
// heal on this log alone (TG-694), so a power change it omits would be invisible there.
var lifecycleTaskTypes = map[string]bool{
	"vzstart": true, "vzstop": true, "vzshutdown": true, "vzreboot": true,
	"qmstart": true, "qmstop": true, "qmshutdown": true, "qmreboot": true,
	"vzsuspend": true, "vzresume": true, "qmsuspend": true, "qmresume": true, "qmreset": true,
	"vzrestore": true, "qmrestore": true, "vzrollback": true, "qmrollback": true,
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
	// selfIdentity and selfExecutions check every task run under TG's own actuation token against TG's own execution
	// record (WithSelfCorrelation, TG-693). nil = not wired: rows pass through unmarked, the pre-TG-693 behaviour.
	selfIdentity   func() string
	selfExecutions ExecutionTimes
}

// ExecutionTimes returns when TG recorded a Proxmox guest-lifecycle execution on guest (action_execution.executed_at)
// inside [from, to]. It reads a record only the ACTUATION plane can write (core/db plane_roles.go), which is what
// makes it a counter-signature for a task the triage plane merely observes in the PVE task log.
type ExecutionTimes func(ctx context.Context, guest string, from, to time.Time) ([]time.Time, error)

// SelfExecutionBefore and SelfExecutionAfter bound how far a TG-recorded execution may sit from the PVE task it
// explains: executed_at − task start must lie in [−SelfExecutionBefore, +SelfExecutionAfter].
//
// MEASURED, 2026-11-08 (TG-693): every lifecycle task by TG's actuation token (`tg-actuate@pve!lane`) still in the
// NL and GR task logs, 2026-08-26 → 2026-11-08, against action_execution. 22 tasks on guests that still exist had an
// execution; executed_at − task start ran from −2.4 s (a GR node clock ahead of the TG box) to +6.7 s (the leaf
// records the execution after it has polled the task to completion; the tasks themselves ran 0–6 s). Exactly one
// task had no execution: 2026-11-08 04:38:58Z on a pool guest, the TG-691 defect (the task ended "WARNINGS: 1",
// the leaf read that as a failure and recorded no execution) — a ledger gap, which is exactly the use that must read
// unexplained. The bounds are the journal domain's (TG-684): a seventeen-fold margin over the measured maximum on the
// late side (a qemu start that runs long) and an NTP-degraded clock on the early side, minutes, not hours.
const (
	SelfExecutionBefore = 30 * time.Second
	SelfExecutionAfter  = 120 * time.Second
)

// WithSelfCorrelation arms the owner's rule (2026-11-07, TG-684; applied to the pve domain by TG-693): a task whose
// principal is TG's actuation identity counts as TG's own ONLY when a TG-recorded Proxmox execution on the SAME guest
// lies inside the tolerance above; otherwise it is marked SelfUnexplained (⇒ suspicious, dominates). identity returns
// TG's actuation identity — derived from the token where this process holds it, the actuation plane's publication
// otherwise ("" while none is published); executions reads the execution record. Fails CLOSED — the read errors, so
// the domain reads indeterminate (TG-668), never self — when either cannot answer for a row that needs it.
func WithSelfCorrelation(identity func() string, executions ExecutionTimes) Option {
	return func(m *Module) {
		m.selfIdentity = identity
		m.selfExecutions = executions
	}
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
	if len(out) == 0 {
		// Affirmative coverage, clean miss: the guest resolved and its task log listed with no lifecycle actor
		// in-window. Emit a coverage marker so covered-but-empty is expressible (REQ-2304 half 2, TG-407) — an
		// empty return would otherwise be indistinguishable from "this domain does not cover the guest", and a
		// guest stopped with no PVE task entry (the intrusion signal) could never reach attributed-suspicious.
		return []attribution.Evidence{attribution.CoverageMarker(m.Domain(), target, until)}, nil
	}
	if err := m.correlateSelf(ctx, target, since, until, out); err != nil {
		return nil, err
	}
	return out, nil
}

// correlateSelf marks each task run under TG's actuation identity SelfCorrelated or SelfUnexplained (TG-693).
//
// TG's PVE identity is always an API-TOKEN principal (`user@realm!tokenid`, derived from the actuation token), so
// only a token-authenticated task can be TG's. Without the identity such a task cannot be told from a stranger's
// token, and without the execution record TG's use of its token cannot be told from a thief's — in either case, for
// a read that holds a token task, the read fails: an error (indeterminate ⇒ HOLD), never a guess in either
// direction. A read holding only password/ticket principals (`root@pam` — an admin, the fault injector) is untouched.
func (m *Module) correlateSelf(ctx context.Context, guest string, since, until time.Time, out []attribution.Evidence) error {
	if m.selfIdentity == nil {
		return nil
	}
	self := strings.TrimSpace(m.selfIdentity())
	if self == "" {
		for _, e := range out {
			if strings.Contains(e.Actor, "!") {
				return fmt.Errorf("pve: TG's own actuation identity is not published (self_identity), so the token task by %q on %q "+
					"cannot be told from TG's own heal — the read is indeterminate, not clean (fail closed, TG-693)", e.Actor, guest)
			}
		}
		return nil
	}
	mine := attribution.SelfRecords(out, self)
	if len(mine) == 0 {
		return nil
	}
	if m.selfExecutions == nil {
		return fmt.Errorf("pve: no execution record is wired, so TG's own tasks on %q cannot be verified (fail closed, TG-693)", guest)
	}
	times, err := m.selfExecutions(ctx, guest, since.Add(-SelfExecutionBefore), until.Add(SelfExecutionAfter))
	if err != nil {
		return fmt.Errorf("pve: TG's execution record for %q is unreadable, so TG's own tasks cannot be verified (fail closed, TG-693): %w", guest, err)
	}
	// ONE TASK PER EXECUTION: the Proxmox leaf issues exactly one lifecycle call per execution, and the measured
	// pairing was one-to-one (22↔22). A second task under TG's token beside one execution reads unexplained.
	attribution.PairSelf(out, mine, times, SelfExecutionBefore, SelfExecutionAfter)
	return nil
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
	Type   string `json:"type"`
	Node   string `json:"node"`
	Name   string `json:"name"`
	Vmid   int    `json:"vmid"`
	Status string `json:"status"`
}

// CheckCanReadAllTasks confirms the token can see other users' tasks. Without Sys.Audit the task log returns only the
// token's own entries, so a deliberate stop would read as no stop at all.
func (m *Module) CheckCanReadAllTasks(ctx context.Context) error {
	body, err := m.get(ctx, "/api2/json/access/permissions?path=/nodes")
	if err != nil {
		return err
	}
	var env struct {
		Data map[string]map[string]int `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("pve: permissions unparseable: %w", err)
	}
	for _, privs := range env.Data {
		if privs["Sys.Audit"] == 1 {
			return nil
		}
	}
	return errors.New("pve: the token lacks Sys.Audit, so it cannot see who stopped a guest")
}

// RanRecently reports whether the hypervisor's own usage history shows the guest running within the given period. A
// running guest has a cpu sample in its history; a stopped one has none. It fails closed on any read or parse error.
func (m *Module) RanRecently(ctx context.Context, name string, within time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	body, err := m.get(ctx, "/api2/json/cluster/resources?type=vm")
	if err != nil {
		return false, err
	}
	var env struct {
		Data []resourcesRow `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return false, fmt.Errorf("pve: cluster resources unparseable: %w", err)
	}
	var row *resourcesRow
	for i := range env.Data {
		if env.Data[i].Name != name {
			continue
		}
		if row != nil {
			return false, fmt.Errorf("pve: guest name %q is ambiguous across the cluster (fail closed)", name)
		}
		row = &env.Data[i]
	}
	if row == nil {
		return false, fmt.Errorf("pve: guest %q not found (fail closed)", name)
	}
	if row.Type != "lxc" && row.Type != "qemu" {
		return false, fmt.Errorf("pve: guest %q has unknown type %q (fail closed)", name, row.Type)
	}
	hist, err := m.get(ctx, fmt.Sprintf("/api2/json/nodes/%s/%s/%d/rrddata?timeframe=day&cf=AVERAGE", row.Node, row.Type, row.Vmid))
	if err != nil {
		return false, err
	}
	var samples struct {
		Data []struct {
			Time int64    `json:"time"`
			CPU  *float64 `json:"cpu"`
		} `json:"data"`
	}
	if err := json.Unmarshal(hist, &samples); err != nil {
		return false, fmt.Errorf("pve: usage history unparseable: %w", err)
	}
	if len(samples.Data) == 0 {
		return false, fmt.Errorf("pve: guest %q has no usage history (fail closed)", name)
	}
	cutoff := time.Now().Add(-within).Unix()
	for _, s := range samples.Data {
		if s.Time >= cutoff && s.CPU != nil {
			return true, nil
		}
	}
	return false, nil
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

// readRetryBackoff is the bounded retry a TRANSIENT read failure gets (TG-652, owner-ruled 2026-11-07: "retry
// briefly, then hold"). On 2026-11-07 the read that would have shown an admin's deliberate shutdown got Proxmox
// 596 from an overloaded node; one retry a second later would very likely have answered. The whole schedule
// fits inside the per-Read timeout (8 s by default) and the Read's context cuts it short, so a dead endpoint
// still fails inside the same budget it always had. Only a 5xx (596 included) or a transport failure is
// retried: a 4xx is a verdict (bad token, no such path) and is returned at once.
var readRetryBackoff = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 3 * time.Second}

// readStatusError is a non-2xx PVE answer. Its text is exactly the pre-TG-652 error string, so logs read as
// before; the status lets do() tell a transient 5xx from a 4xx verdict.
type readStatusError struct {
	status int
	msg    string
}

func (e *readStatusError) Error() string { return e.msg }

// readTransportError is a request that got no HTTP answer at all (connection refused, reset, timeout).
type readTransportError struct{ err error }

func (e *readTransportError) Error() string { return e.err.Error() }
func (e *readTransportError) Unwrap() error { return e.err }

// retryableRead reports whether a failed read may be retried: a 5xx, or no answer at all.
func retryableRead(err error) bool {
	var se *readStatusError
	if errors.As(err, &se) {
		return se.status >= 500
	}
	var te *readTransportError
	return errors.As(err, &te)
}

// do issues one read with the bounded TG-652 retry. When the context ends during a backoff, the LAST read
// error is returned (not the context's), so the caller logs what PVE actually said.
func (m *Module) do(ctx context.Context, method, path string) ([]byte, error) {
	b, err := m.doOnce(ctx, method, path)
	for i := 0; err != nil && retryableRead(err) && i < len(readRetryBackoff); i++ {
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(readRetryBackoff[i]):
		}
		b, err = m.doOnce(ctx, method, path)
	}
	return b, err
}

func (m *Module) doOnce(ctx context.Context, method, path string) ([]byte, error) {
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
		return nil, &readTransportError{err: err}
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
		return nil, &readStatusError{status: resp.StatusCode, msg: fmt.Sprintf("pve: %s %s → %d: %s", method, path, resp.StatusCode, msg)}
	}
	return b, nil
}
