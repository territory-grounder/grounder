// Package proxmox is the loadable Proxmox actuation module (spec/008 REQ-813, T-008-17).
//
// It implements adapters/actuation.Actuator over the NATIVE Proxmox VE REST API — no subprocess, no
// pvesh, no shell (the distroless worker has neither a shell nor pvesh). Every request authenticates with
// the PVEAPIToken scheme against https://{host}:8006/api2/json/ (self-signed cert; the caller supplies the
// TLS policy via WithHTTPClient), and the token is resolved from its secret reference at call time (INV-13,
// never a literal). A lifecycle op names a guest by NAME: the module resolves NAME → node/vmid/type via
// GET /cluster/resources?type=vm (FAIL CLOSED on zero or multiple matches, or an unrecognized type), POSTs
// .../status/{op}, and polls the returned async task UPID to completion.
//
// Lifecycle is DEFAULT-DENY: only recognized ops are expressible, and a hard power-cycle (reboot / reset),
// power-off (shutdown / halt), or permanent deletion (destroy) is clamped to the non-configurable never-auto
// floor — no flag lifts it (INV-09). The only reversible ops are start / stop (suspend/resume do not exist on
// the /lxc/ tree and are refused as unknown); they are permitted only while the process mutation gate is
// enabled. Mutation ships OFF through Phase 0/1, so ReadOnly() is true and Exec refuses at the effect leaf
// (GuardMutation) as defense in depth BEFORE any POST. Every operation still traverses the single
// pre-execution guard chokepoint (INV-21); this module is the effect leaf.
//
// Provenance: [O] INV-09/INV-13/INV-21, spec/008.
package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/safety"
)

// Capability is the declared capability slug this adapter provides.
const Capability = "proxmox"

// taskPollTimeout caps how long the async-task poll waits for a lifecycle task to reach the "stopped"
// terminal state before failing closed with a timeout error.
const taskPollTimeout = 60 * time.Second

// taskPollInterval is the small sleep between task-status polls (ctx-bounded).
const taskPollInterval = 500 * time.Millisecond

// Doer is the minimal HTTP contract; *http.Client satisfies it, and tests inject a fake PVE cluster so the
// real request path runs without a live Proxmox endpoint.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Module is the native Proxmox actuation adapter. Construct with New (read-only by default);
// WithMutation(gate) adds the gated lifecycle path and WithHTTPClient(d) injects the transport.
type Module struct {
	baseURL  string
	tokenRef config.SecretRef
	http     Doer
	// gate is the PROCESS mutation gate this module reads to decide ReadOnly()/GuardMutation. nil ⇒ a
	// read-only module with no lifecycle path. Set only via WithMutation; populating it does NOT turn
	// mutation on — the gate stays the sole key (mutation ships OFF through Phase 0/1).
	gate *safety.Chokepoint
	// allowedGuests is the operator-declared per-guest allowlist (config-not-code, INV-17): only a guest
	// whose NAME is on it may be lifecycle-actuated, even with the gate on. Empty ⇒ NOTHING is allowed
	// (default-deny), mirroring the ssh actuator's allowedUnits. Set only via WithMutation.
	// allowedGuests RESOLVES the allowlist per attempt (TG-232). It was a frozen map, so an operator
	// adopting a guest in the console had to wait for the next worker restart before TG would act on it.
	// nil means DENY EVERYTHING — the default-deny floor, unchanged.
	allowedGuests func(context.Context) []string
}

// ErrGuestNotAllowed refuses a lifecycle op on a guest that is not on the operator-declared allowlist —
// the per-guest scope gate (INV-17), the analog of the ssh actuator's ErrUnitNotAllowed.
var ErrGuestNotAllowed = fmt.Errorf("proxmox: target guest is not on the operator-declared allowlist")

// Option configures a Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP transport (a fake in tests, an *http.Client in production — the caller
// supplies the TLS policy for PVE's self-signed endpoint).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// WithMutation wires the process mutation gate the module reads to decide ReadOnly()/GuardMutation, plus the
// operator-declared per-guest allowlist (config-not-code). Passing it does NOT turn mutation on — the gate
// stays the sole key. Without it the module is read-only and has no lifecycle execution path. An empty
// allowlist means NO guest can be actuated (default-deny), even with the gate on.
func WithMutation(gate *safety.Chokepoint, allowedGuests []string) Option {
	frozen := append([]string(nil), allowedGuests...)
	return WithMutationProvider(gate, func(context.Context) []string { return frozen })
}

// WithMutationProvider is the LIVE form: the allowlist is resolved on every actuation attempt rather than
// frozen at construction (TG-232).
//
// WHY IT MATTERS THAT THIS IS LIVE. Adoption is an operator act performed in the console — someone marks a
// guest approved and expects TG to act on it. The ssh lane already resolves its per-target grant at build
// time, so an adopted systemd/docker entry takes effect at once. This actuator was built ONCE at worker
// boot from a slice, so an adopted GUEST did not become actuatable until the next worker start: the
// manifest said "approved", TG declined, and nothing explained the gap. An operator who cannot tell a
// default-deny refusal from a not-yet-reloaded one learns to distrust the refusal.
//
// The provider is consulted INSIDE the act path, after the mode chokepoint and before any name resolution
// or network call, so the default-deny floor is byte-unchanged: a nil provider or an empty result still
// refuses everything.
func WithMutationProvider(gate *safety.Chokepoint, allowed func(context.Context) []string) Option {
	return func(m *Module) {
		m.gate = gate
		m.allowedGuests = allowed
	}
}

// guestAllowed reports whether this guest may be lifecycle-actuated right now.
//
// A nil provider is DENY, not allow. That direction is the whole point of the per-guest gate: an actuator
// constructed without an allowlist must refuse everything, exactly as an empty allowlist does.
func (m *Module) guestAllowed(ctx context.Context, guestName string) bool {
	if m.allowedGuests == nil {
		return false
	}
	want := strings.TrimSpace(guestName)
	if want == "" {
		return false
	}
	for _, g := range m.allowedGuests(ctx) {
		if strings.TrimSpace(g) == want {
			return true
		}
	}
	return false
}

// New builds a Proxmox module for a base URL (e.g. "https://dc1pve01:8006") and an API-token secret
// reference resolving to a full `user@realm!tokenid=secret` value. Read-only by default.
func New(baseURL string, tokenRef config.SecretRef, opts ...Option) *Module {
	m := &Module{baseURL: strings.TrimRight(baseURL, "/"), tokenRef: tokenRef, http: http.DefaultClient}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Capability implements adapters/actuation.Actuator.
func (m *Module) Capability() string { return Capability }

// ReadOnly reports whether the module can only observe. It is true UNLESS the process mutation gate is wired
// AND enabled. Mutation ships OFF — the gate is disabled through all of Phase 0/1 — so this returns true in
// every production and test path, except one that constructs a test-only enabled gate.
func (m *Module) ReadOnly() bool { return !(m.gate != nil && m.gate.MayActuate()) }

// compile-time proof the module satisfies the stable actuation interface.
var _ actuation.Actuator = (*Module)(nil)

// Lifecycle is a DEFAULT-DENY authorization gate (mirroring the sibling kubernetes actuator's allowlist): an
// operation is permitted only if it is explicitly recognized. A hard power-cycle (reboot / reset), a power-off
// (shutdown / halt), or a permanent deletion (destroy) is on the non-configurable never-auto floor — no gate
// lifts it (INV-09). reset and destroy are the predecessor's `qm reset` (POLL_PAUSE) and `qm destroy`
// (irreversible floor); a token-EXACT floor over only reboot/shutdown/halt would let the harsher reset/destroy
// dodge the clamp and invert the predecessor's danger ordering. The ONLY reversible ops are start / stop
// (suspend/resume do not exist on the /lxc/ tree, so they fall through to default-deny); they are refused
// while the mutation gate is off. Any UNKNOWN op — including a vmid/status path form like "101/status/reboot"
// that would slip past a token-exact floor — is refused (default deny). This no longer builds an argv; it only
// validates and authorizes the op.
func (m *Module) Lifecycle(op string) error {
	op = strings.ToLower(strings.TrimSpace(op)) // normalize so a case/whitespace variant cannot dodge the gate
	switch op {
	case "reboot", "shutdown", "halt", "reset", "destroy":
		return fmt.Errorf("proxmox: %q is clamped to the non-configurable never-auto floor and can never auto-execute", op)
	case "start", "stop":
		if safety.IsNeverAuto(op) { // belt-and-suspenders: honor the shared floor if one of these is ever added to it
			return fmt.Errorf("proxmox: %q is clamped to the non-configurable never-auto floor and can never auto-execute", op)
		}
		if !(m.gate != nil && m.gate.MayActuate()) {
			return fmt.Errorf("proxmox: lifecycle operations are disabled (the mutation gate is off)")
		}
		return nil
	default:
		return fmt.Errorf("proxmox: unsupported lifecycle operation %q (default-deny)", op)
	}
}

// Exec routes argv=[op, guestName] as a native lifecycle mutation. It refuses at the effect leaf while the
// gate is off (defense in depth) BEFORE any network call, validates the op through the floor clamp, resolves
// the guest NAME to its node/vmid/type, POSTs .../status/{op}, and polls the async task UPID to completion.
// A non-OK task exitstatus is a Result with ExitCode 1 (not a Go error); a Go error is returned only for a
// transport/auth/resolution/timeout failure. No shell is involved anywhere.
func (m *Module) Exec(ctx context.Context, argv []string, stdin []byte) (actuation.Result, error) {
	if len(argv) < 2 {
		return actuation.Result{}, actuation.ErrEmptyArgv
	}
	op, guestName := argv[0], argv[1]
	// Defense in depth at the leaf: refuse while the gate is off, BEFORE any POST (INV-09). A read-only
	// module (no gate) has no mutating path at all.
	if m.gate == nil {
		return actuation.Result{}, safety.ErrMutationDisabled
	}
	if err := m.gate.GuardMutation(); err != nil {
		return actuation.Result{}, err // gate OFF ⇒ safety.ErrMutationDisabled — nothing mutates while the key is out
	}
	if err := m.Lifecycle(op); err != nil {
		return actuation.Result{}, err
	}
	// Per-guest scope gate (INV-17): only an operator-allowlisted guest may be actuated, even with the gate
	// on and the op reversible. Checked on the RAW proposed name BEFORE any resolution or network call, so a
	// non-allowlisted (or empty-allowlist default-deny) target never reaches the cluster.
	if !m.guestAllowed(ctx, guestName) {
		return actuation.Result{}, ErrGuestNotAllowed
	}
	node, vmid, segment, err := m.resolveGuest(ctx, guestName)
	if err != nil {
		return actuation.Result{}, err
	}
	upid, err := m.postStatus(ctx, node, segment, vmid, strings.ToLower(strings.TrimSpace(op)))
	if err != nil {
		// GOAL-STATE IDEMPOTENCE. Proxmox answers "CT <vmid> already running" with HTTP 500 when a start is
		// issued against a guest that is already up. That is the DESIRED STATE, not a failure: the reason TG
		// acted (the guest was down) no longer holds by the time the call lands. It happens whenever something
		// else recovers the guest first — an operator, a restore timer, or the fault injector's own
		// auto-restore — and it is a genuine race, not a defect.
		//
		// Reporting it as a failed mutation is WRONG in a way that corrupts the evidence base: measured live,
		// 50 of 72 refusals in a week were this race, each recorded as "execute failed" while the estate was
		// in exactly the state TG wanted. That inflates the apparent failure rate and, worse, denies A3 a heal
		// TG effectively achieved.
		//
		// Matching on the API's own message rather than the bare 500: a 500 alone can mean many things, and
		// treating every one as success would hide real failures. The guest's state is then RE-READ below, so
		// this never trusts the message alone — it converts a claim into a verified observation.
		if isAlreadyInGoalState(err, op) {
			ok, verr := m.guestIsRunning(ctx, node, segment, vmid)
			if verr == nil && ok == wantRunning(op) {
				// NoOp is the load-bearing half. Reporting this as a plain success (ExitCode 0, NoOp unset) made it
				// INDISTINGUISHABLE downstream from a real mutation: the interceptor read only the exit code, the
				// verifier then observed the guest already in goal state and scored `match`, and `match` is the only
				// promoting graduation outcome — so an op-class earned autonomy credit for a start it never performed.
				return actuation.Result{Stdout: []byte("no-op: guest already in the requested state (" + op + ")"), NoOp: true}, nil
			}
		}
		return actuation.Result{}, err
	}
	exitstatus, err := m.pollTask(ctx, node, upid)
	if err != nil {
		return actuation.Result{}, err
	}
	res := actuation.Result{Stdout: []byte(upid + " " + exitstatus)}
	if exitstatus != "OK" {
		res.ExitCode = 1 // a task that finished with a non-OK exitstatus is a failed mutation, not a Go error
	}
	return res, nil
}

// isAlreadyInGoalState reports whether a Proxmox error means the guest is ALREADY in the state the op wants.
// Keyed on the API's own wording plus the op, never on the bare status code: a 500 can mean many things, and
// treating all of them as success would hide real failures.
func isAlreadyInGoalState(err error, op string) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch strings.ToLower(strings.TrimSpace(op)) {
	case "start":
		return strings.Contains(msg, "already running")
	case "stop", "shutdown":
		return strings.Contains(msg, "not running")
	}
	return false
}

// wantRunning reports the guest state the op is trying to reach.
func wantRunning(op string) bool { return strings.EqualFold(strings.TrimSpace(op), "start") }

// guestIsRunning re-reads the guest's live status. This is what makes the goal-state short-circuit an
// OBSERVATION rather than a guess: the API's message is only a hint, and the estate is asked directly.
func (m *Module) guestIsRunning(ctx context.Context, node, segment string, vmid int) (bool, error) {
	body, err := m.do(ctx, http.MethodGet, fmt.Sprintf("/api2/json/nodes/%s/%s/%d/status/current", node, segment, vmid), nil)
	if err != nil {
		return false, err
	}
	var res struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return false, fmt.Errorf("proxmox: malformed status response: %w", err)
	}
	return strings.EqualFold(strings.TrimSpace(res.Data.Status), "running"), nil
}

// ExecLog derives the compensating rollback for a lifecycle command the interceptor has (gated) executed,
// bound to the authorizing action id (INV-07) — it satisfies core/actuate.ExecRecorder so the interceptor
// records an undoable action. A container lifecycle is its own inverse: start↔stop. The forward is the
// canonical [op, guest]; the rollback is [inverse-op, guest]. A read-only module (no gate) or an argv that
// is not a recognized reversible op yields no log (nothing to record). No shell is involved.
func (m *Module) ExecLog(actionID string, command []string) (forward, rollback []string, err error) {
	if m.gate == nil || len(command) < 2 {
		return nil, nil, nil // read-only module, or not a proxmox [op, guest] command — nothing to record
	}
	op := strings.ToLower(strings.TrimSpace(command[0]))
	guest := command[1]
	var inverse string
	switch op {
	case "start":
		inverse = "stop"
	case "stop":
		inverse = "start"
	default:
		return nil, nil, nil // not a recognized reversible lifecycle op (floor/unknown) — nothing to record
	}
	return []string{op, guest}, []string{inverse, guest}, nil
}

// resourcesRow is one row of GET /cluster/resources?type=vm.
type resourcesRow struct {
	Type   string `json:"type"` // "lxc" | "qemu"
	Node   string `json:"node"` // the hypervisor node the guest is placed on
	Name   string `json:"name"`
	Vmid   int    `json:"vmid"`
	Status string `json:"status"`
}

// resolveGuest resolves a guest NAME to its node, vmid, and API path segment ("lxc"|"qemu") via a single
// authenticated GET of the cluster resources. It FAILS CLOSED on zero or multiple name matches (an ambiguous
// or absent guest is never actuated) and on a type that is neither lxc nor qemu.
func (m *Module) resolveGuest(ctx context.Context, name string) (node string, vmid int, segment string, err error) {
	target := strings.TrimSpace(name)
	if target == "" {
		return "", 0, "", fmt.Errorf("proxmox: empty guest name — failing closed")
	}
	body, err := m.get(ctx, "/api2/json/cluster/resources?type=vm")
	if err != nil {
		return "", 0, "", err
	}
	var res struct {
		Data []resourcesRow `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", 0, "", fmt.Errorf("proxmox: malformed cluster resources: %w", err)
	}
	var match resourcesRow
	count := 0
	for _, r := range res.Data {
		if strings.TrimSpace(r.Name) == target {
			match = r
			count++
		}
	}
	if count != 1 {
		return "", 0, "", fmt.Errorf("proxmox: guest %q resolved to %d rows (need exactly 1) — failing closed", name, count)
	}
	segment, err = guestSegment(match.Type)
	if err != nil {
		return "", 0, "", err
	}
	return strings.TrimSpace(match.Node), match.Vmid, segment, nil
}

// guestSegment maps a resource type to its API path segment: lxc→"lxc", qemu→"qemu". Any other type fails
// closed (a storage/node/unknown row is never actuated).
func guestSegment(typ string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "lxc":
		return "lxc", nil
	case "qemu":
		return "qemu", nil
	default:
		return "", fmt.Errorf("proxmox: guest type %q is neither lxc nor qemu — failing closed", typ)
	}
}

// postStatus POSTs .../status/{op} (no body) and returns the async task UPID from the {data: "<UPID>"} envelope.
func (m *Module) postStatus(ctx context.Context, node, segment string, vmid int, op string) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/%s/%d/status/%s", node, segment, vmid, op)
	body, err := m.do(ctx, http.MethodPost, path, nil)
	if err != nil {
		return "", err
	}
	var res struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("proxmox: malformed task-start response: %w", err)
	}
	upid := strings.TrimSpace(res.Data)
	if upid == "" {
		return "", fmt.Errorf("proxmox: POST %s returned no task UPID", path)
	}
	return upid, nil
}

// pollTask polls GET /nodes/{node}/tasks/{upid}/status (ctx-bounded, small sleep between polls, capped at
// taskPollTimeout) until the task reaches the terminal "stopped" status, then returns its exitstatus. A
// transport/auth error or a timeout is a Go error; the exitstatus itself (OK or a failure message) is returned
// to the caller, which maps it to the Result exit code.
func (m *Module) pollTask(ctx context.Context, node, upid string) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/tasks/%s/status", node, upid)
	deadline := time.Now().Add(taskPollTimeout)
	for {
		body, err := m.get(ctx, path)
		if err != nil {
			return "", err
		}
		var res struct {
			Data struct {
				Status     string `json:"status"`
				ExitStatus string `json:"exitstatus"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return "", fmt.Errorf("proxmox: malformed task status: %w", err)
		}
		if strings.EqualFold(strings.TrimSpace(res.Data.Status), "stopped") {
			return strings.TrimSpace(res.Data.ExitStatus), nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("proxmox: task %s did not reach a terminal state within %s — failing closed", upid, taskPollTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(taskPollInterval):
		}
	}
}

// do issues an authenticated request against the PVE API using the PVEAPIToken scheme; the token is resolved
// from its secret reference at call time (INV-13). A non-2xx status is an error.
func (m *Module) do(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	token, err := m.tokenRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("proxmox: resolve token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+token)
	req.Header.Set("Accept", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, rerr := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
	if rerr != nil {
		return nil, fmt.Errorf("proxmox: %s %s: read response body: %w", method, path, rerr) // fail closed on a partial read
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Redact any token the API/proxy might echo before it reaches a log (INV-13); the request's own
		// Authorization header is never included in the error to begin with.
		return nil, fmt.Errorf("proxmox: %s %s: status %d: %s", method, path, resp.StatusCode, redactToken(strings.TrimSpace(string(out))))
	}
	return out, nil
}

// maxRespBody bounds a single API response TG will read into memory.
const maxRespBody = 1 << 20 // 1 MiB

// tokenRedactRE matches a PVEAPIToken value (up to whitespace or a quote) so redactToken can strip it from
// any error body TG surfaces — a token must never reach the logs (INV-13).
var tokenRedactRE = regexp.MustCompile(`PVEAPIToken=[^\s"']+`)

// redactToken strips any leaked PVEAPIToken value from a string (an API error body / proxy error page).
func redactToken(s string) string { return tokenRedactRE.ReplaceAllString(s, "PVEAPIToken=<redacted>") }

// get is the read helper (an authenticated GET).
func (m *Module) get(ctx context.Context, path string) ([]byte, error) {
	return m.do(ctx, http.MethodGet, path, nil)
}
