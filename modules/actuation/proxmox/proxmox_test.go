package proxmox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/safety"
)

const testBaseURL = "https://pve01:8006"

// tokenRef returns a SecretRef whose env-backed secret is set for the calling test.
func tokenRef(t *testing.T) config.SecretRef {
	t.Helper()
	t.Setenv("TG_TEST_PVE_TOKEN", "root@pam!tg=uuid")
	return config.SecretRef("env:TG_TEST_PVE_TOKEN")
}

// fakeDoer is a canned PVE cluster: it serves /cluster/resources (a guest list), /status/{op} (a UPID), and
// /tasks/{upid}/status (a terminal task), and records every request path so a test can assert the exact
// native URLs the module issued.
type fakeDoer struct {
	seenURLs   []string
	resources  string // JSON array served as {"data": <resources>}
	upid       string // the UPID returned by a status POST
	status     string // task status, default "stopped"
	exitstatus string // task exitstatus (e.g. "OK")
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.seenURLs = append(f.seenURLs, req.URL.Path)
	p := req.URL.Path
	switch {
	case strings.HasSuffix(p, "/cluster/resources"):
		return canned(200, `{"data":`+f.resources+`}`), nil
	case strings.Contains(p, "/tasks/"): // /nodes/{node}/tasks/{upid}/status
		st := f.status
		if st == "" {
			st = "stopped"
		}
		return canned(200, fmt.Sprintf(`{"data":{"status":%q,"exitstatus":%q}}`, st, f.exitstatus)), nil
	case strings.Contains(p, "/status/"): // POST /nodes/{node}/{lxc|qemu}/{vmid}/status/{op}
		return canned(200, fmt.Sprintf(`{"data":%q}`, f.upid)), nil
	default:
		return canned(404, `{"data":null}`), nil
	}
}

func (f *fakeDoer) sawPath(p string) bool {
	for _, s := range f.seenURLs {
		if s == p {
			return true
		}
	}
	return false
}

// sawStatusPost reports whether any recorded path is a lifecycle status POST (not a task-status GET).
func (f *fakeDoer) sawStatusPost() bool {
	for _, s := range f.seenURLs {
		if strings.Contains(s, "/status/") && !strings.Contains(s, "/tasks/") {
			return true
		}
	}
	return false
}

func canned(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

// enabledModule builds a mutation-configured module with an actuating chokepoint (mutation ON, test-only) and
// the given fake transport.
func enabledModule(t *testing.T, f *fakeDoer) *Module {
	t.Helper()
	return New(testBaseURL, tokenRef(t), WithHTTPClient(f), WithMutation(safety.NewActuatingChokepoint(), []string{"web"}))
}

// oneLXC is a canned single-lxc cluster whose start task finishes OK.
func oneLXC() *fakeDoer {
	return &fakeDoer{
		resources:  `[{"name":"web","node":"pve01","vmid":101,"type":"lxc","status":"running"}]`,
		upid:       "UPID:pve01:0000ABCD:00001234:60ABCDEF:vzstart:101:root@pam:",
		status:     "stopped",
		exitstatus: "OK",
	}
}

// Floor ops (reboot/reset/destroy/shutdown/halt) are clamped to the never-auto floor whether or not the gate
// is enabled — no flag lifts them (INV-09).
func TestFloorOpsClampedWithGateOnOrOff(t *testing.T) {
	// gate OFF (read-only module).
	ro := New(testBaseURL, tokenRef(t))
	// gate ON.
	on := enabledModule(t, oneLXC())
	for _, m := range []*Module{ro, on} {
		for _, op := range []string{"reboot", "reset", "destroy", "shutdown", "halt"} {
			if err := m.Lifecycle(op); err == nil || !strings.Contains(err.Error(), "never-auto floor") {
				t.Errorf("%q must be clamped to the floor, got %v", op, err)
			}
		}
	}
}

// A capitalized/whitespaced floor op must still be clamped, even with the gate enabled — regression for the
// case-sensitivity bypass an adversarial review found.
func TestFloorClampIsCaseInsensitive(t *testing.T) {
	m := enabledModule(t, oneLXC())
	for _, op := range []string{"Reboot", "REBOOT", " reboot ", "Shutdown", "HaLt", "Reset", " DESTROY "} {
		if err := m.Lifecycle(op); err == nil || !strings.Contains(err.Error(), "never-auto floor") {
			t.Errorf("floor op %q (gate enabled) must be clamped, got %v", op, err)
		}
	}
}

// start/stop are the only reversible ops, and only while the gate is enabled; suspend/resume no longer exist
// on the /lxc/ tree and fall through to default-deny; an unknown/path-form op is refused (default deny).
func TestStartStopGatedAndUnknownRefused(t *testing.T) {
	// gate off (no gate, and an explicit disabled gate) ⇒ start/stop refused.
	for _, m := range []*Module{
		New(testBaseURL, tokenRef(t)),
		New(testBaseURL, tokenRef(t), WithMutation(safety.NewReadOnlyChokepoint(), nil)),
	} {
		for _, op := range []string{"start", "stop"} {
			if err := m.Lifecycle(op); err == nil || !strings.Contains(err.Error(), "disabled") {
				t.Errorf("%q must be refused while the gate is off, got %v", op, err)
			}
		}
	}
	// gate on ⇒ start/stop permitted; suspend/resume/unknown/path-form refused (default deny).
	on := enabledModule(t, oneLXC())
	for _, op := range []string{"start", "stop"} {
		if err := on.Lifecycle(op); err != nil {
			t.Errorf("%q must be permitted with the gate enabled, got %v", op, err)
		}
	}
	for _, op := range []string{"suspend", "resume", "101/status/reboot", "migrate", "clone", "snapshot"} {
		if err := on.Lifecycle(op); err == nil {
			t.Errorf("op %q must be refused (default-deny), got nil error", op)
		}
	}
}

// ReadOnly is gate-aware: true when the gate is nil or disabled, false only with a proven, enabled gate.
func TestReadOnlyIsGateAware(t *testing.T) {
	if !New(testBaseURL, tokenRef(t)).ReadOnly() {
		t.Fatal("a module with no gate must be read-only")
	}
	// gate OFF (read-only chokepoint): a mutation-configured module must still report read-only.
	if !New(testBaseURL, tokenRef(t), WithMutation(safety.NewReadOnlyChokepoint(), nil)).ReadOnly() {
		t.Fatal("gate OFF: a mutation-configured module must still report read-only")
	}
	// gate ON (actuating chokepoint): the module must report NOT read-only.
	on := New(testBaseURL, tokenRef(t), WithMutation(safety.NewActuatingChokepoint(), nil))
	if on.ReadOnly() {
		t.Fatal("gate ON: the module must report NOT read-only")
	}
	if on.Capability() != "proxmox" {
		t.Fatalf("capability wrong: %q", on.Capability())
	}
}

// The native happy path: with an enabled gate, Exec(["start","web"]) resolves the guest via /cluster/resources,
// POSTs to the exact /nodes/<node>/lxc/<vmid>/status/start path, polls the task UPID, and returns ExitCode 0.
func TestExecStartResolvesGuestPostsCorrectPathAndPolls(t *testing.T) {
	f := oneLXC()
	m := enabledModule(t, f)

	res, err := m.Exec(context.Background(), []string{"start", "web"}, nil)
	if err != nil {
		t.Fatalf("start must resolve + POST + poll: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("an OK task must yield ExitCode 0, got %d (stdout %q)", res.ExitCode, res.Stdout)
	}
	if !strings.Contains(string(res.Stdout), f.upid) || !strings.Contains(string(res.Stdout), "OK") {
		t.Fatalf("stdout must carry the UPID + exitstatus, got %q", res.Stdout)
	}
	// the guest was resolved and the status POST hit the correct native path (lxc segment, resolved vmid).
	if !f.sawPath("/api2/json/cluster/resources") {
		t.Fatalf("must resolve via /cluster/resources, saw %v", f.seenURLs)
	}
	wantPost := "/api2/json/nodes/pve01/lxc/101/status/start"
	if !f.sawPath(wantPost) {
		t.Fatalf("must POST to %q, saw %v", wantPost, f.seenURLs)
	}
	wantPoll := "/api2/json/nodes/pve01/tasks/" + f.upid + "/status"
	if !f.sawPath(wantPoll) {
		t.Fatalf("must poll the task at %q, saw %v", wantPoll, f.seenURLs)
	}
}

// A non-OK task exitstatus is a Result with ExitCode 1, not a Go error.
func TestExecNonOKTaskYieldsExitCode1(t *testing.T) {
	f := oneLXC()
	f.exitstatus = "start failed: config error"
	m := enabledModule(t, f)

	res, err := m.Exec(context.Background(), []string{"start", "web"}, nil)
	if err != nil {
		t.Fatalf("a non-OK task is a Result, not a Go error: %v", err)
	}
	if res.ExitCode != 1 {
		t.Fatalf("a non-OK task must yield ExitCode 1, got %d", res.ExitCode)
	}
}

// A guest name that matches zero rows fails closed and never POSTs a status change.
func TestExecZeroMatchFailsClosed(t *testing.T) {
	f := oneLXC()
	f.resources = `[{"name":"other","node":"pve01","vmid":102,"type":"lxc","status":"running"}]`
	m := enabledModule(t, f)

	if _, err := m.Exec(context.Background(), []string{"start", "web"}, nil); err == nil {
		t.Fatal("a guest matching zero rows must fail closed")
	}
	if f.sawStatusPost() {
		t.Fatalf("a failed resolution must not POST a status change, saw %v", f.seenURLs)
	}
}

// A guest name that matches multiple rows fails closed (ambiguous target is never actuated).
func TestExecMultipleMatchFailsClosed(t *testing.T) {
	f := oneLXC()
	f.resources = `[{"name":"web","node":"pve01","vmid":101,"type":"lxc"},{"name":"web","node":"pve02","vmid":201,"type":"lxc"}]`
	m := enabledModule(t, f)

	if _, err := m.Exec(context.Background(), []string{"start", "web"}, nil); err == nil {
		t.Fatal("a guest matching multiple rows must fail closed")
	}
	if f.sawStatusPost() {
		t.Fatalf("an ambiguous resolution must not POST a status change, saw %v", f.seenURLs)
	}
}

// A row whose type is neither lxc nor qemu fails closed (no path segment to actuate).
func TestExecUnknownGuestTypeFailsClosed(t *testing.T) {
	f := oneLXC()
	f.resources = `[{"name":"web","node":"pve01","vmid":101,"type":"storage"}]`
	m := enabledModule(t, f)

	if _, err := m.Exec(context.Background(), []string{"start", "web"}, nil); err == nil {
		t.Fatal("a non-lxc/qemu guest type must fail closed")
	}
	if f.sawStatusPost() {
		t.Fatalf("an unresolvable type must not POST a status change, saw %v", f.seenURLs)
	}
}

// Defense in depth: with the gate off, Exec refuses with ErrMutationDisabled BEFORE touching the network —
// both for a read-only module (no gate) and an explicit disabled gate.
func TestExecGateOffRefusesBeforeAnyNetworkCall(t *testing.T) {
	f := oneLXC()
	if _, err := New(testBaseURL, tokenRef(t), WithHTTPClient(f)).Exec(context.Background(), []string{"start", "web"}, nil); err != safety.ErrMutationDisabled {
		t.Fatalf("a read-only module must refuse with ErrMutationDisabled, got %v", err)
	}
	if len(f.seenURLs) != 0 {
		t.Fatalf("gate OFF must not touch the network, saw %v", f.seenURLs)
	}
	f2 := oneLXC()
	m := New(testBaseURL, tokenRef(t), WithHTTPClient(f2), WithMutation(safety.NewReadOnlyChokepoint(), nil)) // gate OFF
	if _, err := m.Exec(context.Background(), []string{"start", "web"}, nil); err != safety.ErrMutationDisabled {
		t.Fatalf("a disabled gate must refuse with ErrMutationDisabled, got %v", err)
	}
	if len(f2.seenURLs) != 0 {
		t.Fatalf("a disabled gate must not touch the network, saw %v", f2.seenURLs)
	}
}

// A floor op through Exec (gate enabled) is refused at the clamp and never touches the network.
func TestExecFloorOpRefusedNoNetwork(t *testing.T) {
	f := oneLXC()
	m := enabledModule(t, f)
	if _, err := m.Exec(context.Background(), []string{"reboot", "web"}, nil); err == nil || !strings.Contains(err.Error(), "never-auto floor") {
		t.Fatalf("reboot via Exec must be clamped, got %v", err)
	}
	if len(f.seenURLs) != 0 {
		t.Fatalf("a floored op must not touch the network, saw %v", f.seenURLs)
	}
}

// Empty or short argv (missing the guest name) is rejected with ErrEmptyArgv, ahead of the gate check.
func TestExecEmptyOrShortArgvRejected(t *testing.T) {
	m := enabledModule(t, oneLXC())
	for _, argv := range [][]string{nil, {}, {"start"}} {
		if _, err := m.Exec(context.Background(), argv, nil); err != actuation.ErrEmptyArgv {
			t.Fatalf("argv %v must be rejected with ErrEmptyArgv, got %v", argv, err)
		}
	}
}

// A guest NOT on the operator allowlist is refused BEFORE any resolution/POST — the per-guest scope gate.
func TestExecRefusesNonAllowlistedGuest(t *testing.T) {
	m := New(testBaseURL, tokenRef(t), WithHTTPClient(oneLXC()), WithMutation(safety.NewActuatingChokepoint(), []string{"web"}))
	if _, err := m.Exec(context.Background(), []string{"start", "database"}, nil); !errors.Is(err, ErrGuestNotAllowed) {
		t.Fatalf("a non-allowlisted guest must be refused with ErrGuestNotAllowed, got %v", err)
	}
	// an EMPTY allowlist default-denies even the resolvable guest.
	m2 := New(testBaseURL, tokenRef(t), WithHTTPClient(oneLXC()), WithMutation(safety.NewActuatingChokepoint(), nil))
	if _, err := m2.Exec(context.Background(), []string{"start", "web"}, nil); !errors.Is(err, ErrGuestNotAllowed) {
		t.Fatalf("an empty allowlist must default-deny, got %v", err)
	}
}

// ExecLog derives the compensating rollback (start<->stop) bound to the action id (INV-07); a floor/unknown
// op and a read-only module record nothing.
func TestExecLogDerivesInverse(t *testing.T) {
	m := enabledModule(t, oneLXC())
	fwd, rb, err := m.ExecLog("act-1", []string{"start", "web"})
	if err != nil || len(fwd) != 2 || fwd[0] != "start" || len(rb) != 2 || rb[0] != "stop" || rb[1] != "web" {
		t.Fatalf("start rollback must be stop web: fwd=%v rb=%v err=%v", fwd, rb, err)
	}
	if _, rb, _ := m.ExecLog("act-2", []string{"stop", "web"}); rb[0] != "start" {
		t.Fatalf("stop rollback must be start, got %v", rb)
	}
	if f, r, _ := m.ExecLog("act-3", []string{"reboot", "web"}); f != nil || r != nil {
		t.Fatalf("a non-reversible op must record no rollback, got fwd=%v rb=%v", f, r)
	}
	if f, r, _ := New(testBaseURL, tokenRef(t)).ExecLog("a", []string{"start", "web"}); f != nil || r != nil {
		t.Fatalf("a read-only module must record no rollback")
	}
}
