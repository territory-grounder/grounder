package ssh

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
	"github.com/territory-grounder/grounder/modules"
)

// noObserved is a wired post-execution observer that observes nothing (an empty, non-nil observation) — it
// satisfies the interceptor's verifiability gate so the canary can execute through the chain and verify as
// match.
func noObserved(context.Context) []verify.ObservedAlert { return []verify.ObservedAlert{} }

func canaryManifest(t *testing.T) *manifest.ActionManifest {
	t.Helper()
	m, err := manifest.New(manifest.Action{Target: "web01", OpClass: OpClassRestartService, Op: "systemctl restart nginx", Reversible: true}, safety.BandAuto, "plan#822", "pred#822")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// canaryRequest is a fully-admissible mutating request for the canary restart-service op on an allowlisted
// unit (gated, reversible, non-floor, bound evidence, benign territory).
func canaryRequest(t *testing.T) actuate.Request {
	t.Helper()
	return actuate.Request{
		Manifest: canaryManifest(t),
		Gated:    true,
		Argv:     []string{"systemctl", "restart", "nginx"},
		Evidence: []actuate.Evidence{{ToolResultID: "tr-1", Captured: true, Successful: true, Recent: true, Relevant: true}},
		Observe:  noObserved,
		Band:     safety.BandAuto, // fresh per-incident band (TG-126): AUTO admits at 1b
	}
}

// ReadOnly is gate-aware: a mutation-configured module is read-only while the gate is off (the whole of
// Phase 0/1) and reports NOT read-only only once the proven gate is turned on.
func TestReadOnlyIsGateAware(t *testing.T) {
	// a module with no mutation config is always read-only.
	if !New("web01", "svc-agent", &fakeRunner{}).ReadOnly() {
		t.Fatal("a module with no mutation config must be read-only")
	}
	// gate OFF (read-only chokepoint): a mutation-configured module must still report read-only.
	if !New("web01", "svc-agent", &fakeRunner{}, WithMutation(safety.NewReadOnlyChokepoint(), []string{"nginx"}, nil)).ReadOnly() {
		t.Fatal("gate OFF: a mutation-configured module must still report read-only")
	}
	// gate ON (actuating chokepoint) + reversible allowlist: the module must report NOT read-only.
	if New("web01", "svc-agent", &fakeRunner{}, WithMutation(safety.NewActuatingChokepoint(), []string{"nginx"}, nil)).ReadOnly() {
		t.Fatal("gate ON + reversible allowlist: the module must report NOT read-only")
	}
}

// The canary mutating path builds the EXACT fixed argv, wrapped in the host-key-verified non-interactive ssh
// invocation with each remote argument POSIX-quoted, and records an action_id-bound execution_log (INV-07).
func TestCanaryMutatingArgvConstruction(t *testing.T) {
	f := &fakeRunner{}
	m := New("web01", "svc-agent", f, WithMutation(safety.NewActuatingChokepoint(), []string{"nginx"}, nil))

	_, log, err := m.Mutate(context.Background(), "act-822", OpClassRestartService, "nginx")
	if err != nil {
		t.Fatalf("the canary must resolve and run: %v", err)
	}
	got := strings.Join(f.argv, " ")
	if f.argv[0] != "ssh" || !strings.Contains(got, "StrictHostKeyChecking=yes") {
		t.Fatalf("must be a host-key-verified ssh invocation, got %q", got)
	}
	for _, bad := range []string{"StrictHostKeyChecking=no", "-t", "sh -c"} {
		if strings.Contains(got, bad) {
			t.Fatalf("must not express %q, got %q", bad, got)
		}
	}
	if f.argv[len(f.argv)-1] != `'systemctl' 'restart' 'nginx'` {
		t.Fatalf("remote command must be the exact POSIX-quoted canary argv, got %q", f.argv[len(f.argv)-1])
	}
	if log.ActionID != "act-822" || len(log.Rollback) == 0 {
		t.Fatalf("the execution_log must bind the rollback to the action id: %+v", log)
	}
}

// Gate OFF ⇒ ReadOnly true ⇒ the interceptor refuses at GuardMutation ⇒ ZERO exec on the runner; and the
// module's own mutating entry refuses too. This is the "mutation stays off" inertness proof.
func TestGateOffMutatingPathIsInert(t *testing.T) {
	f := &fakeRunner{}
	g := safety.NewReadOnlyChokepoint() // OFF (never actuates)
	m := New("web01", "svc-agent", f, WithMutation(g, []string{"nginx"}, nil))

	if !m.ReadOnly() {
		t.Fatal("gate OFF must report read-only")
	}
	if _, _, err := m.Mutate(context.Background(), "act-1", OpClassRestartService, "nginx"); !errors.Is(err, safety.ErrMutationDisabled) {
		t.Fatalf("gate OFF: Mutate must refuse with ErrMutationDisabled, got %v", err)
	}
	// through the wired interceptor: the fully-admissible canary is refused, nothing executes.
	i := actuate.NewInterceptor(g, m, audit.NewLedger())
	out, err := i.Do(context.Background(), canaryRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Refused || out.Executed {
		t.Fatalf("gate OFF: the interceptor must refuse the canary: %+v", out)
	}
	if f.argv != nil {
		t.Fatalf("gate OFF: NOTHING may reach the runner, got %v", f.argv)
	}
}

// An unregistered OR disabled actuator has NO execution path (INV-17) — Resolve returns ErrNoExecutionPath.
func TestSSHActuationRegisteredDisabledHasNoExecutionPath(t *testing.T) {
	reg := modules.NewRegistry()
	m := New("", "", &fakeRunner{}, WithMutation(safety.NewReadOnlyChokepoint(), []string{"nginx"}, nil))
	if err := reg.Register(modules.Registration{Surface: modules.SurfaceActuation, SourceType: m.Capability(), Capability: "actuation.ssh", Enabled: false, Adapter: m}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Resolve(modules.SurfaceActuation, m.Capability()); !errors.Is(err, modules.ErrNoExecutionPath) {
		t.Fatalf("a DISABLED actuator must resolve to ErrNoExecutionPath, got %v", err)
	}
	if _, err := reg.Resolve(modules.SurfaceActuation, "unregistered"); !errors.Is(err, modules.ErrNoExecutionPath) {
		t.Fatalf("an UNREGISTERED actuator must resolve to ErrNoExecutionPath, got %v", err)
	}
}

// A never-auto floor op_class can never resolve — even with the gate ON and even if it was mistakenly added
// to the reversible allowlist (INV-09). The floor is not tunable by this change.
func TestNeverAutoFloorRefusedEvenIfAllowlisted(t *testing.T) {
	f := &fakeRunner{}
	m := New("web01", "svc-agent", f, WithMutation(safety.NewActuatingChokepoint(), []string{"nginx"}, nil))

	if _, _, err := m.resolveOp("reboot", "nginx"); !errors.Is(err, ErrNeverAutoFloor) {
		t.Fatalf("a never-auto floor op_class must be refused, got %v", err)
	}
	// mis-allowlist a floor class onto the reversible set — the floor STILL refuses it.
	m.reversible["reboot"] = true
	if _, _, err := m.resolveOp("reboot", "nginx"); !errors.Is(err, ErrNeverAutoFloor) {
		t.Fatalf("a mis-allowlisted floor op_class must STILL be floored, got %v", err)
	}
	if f.argv != nil {
		t.Fatalf("a floored op must never reach the runner, got %v", f.argv)
	}
}

// A stateful workload (DB/queue/store) restart is refused even if the unit is allowlisted — data/quorum-loss
// hard floor.
func TestStatefulUnitRefusedEvenIfAllowlisted(t *testing.T) {
	m := New("db01", "svc-agent", &fakeRunner{}, WithMutation(safety.NewActuatingChokepoint(), []string{"postgresql", "nginx"}, nil))
	if _, _, err := m.resolveOp(OpClassRestartService, "postgresql"); !errors.Is(err, ErrStatefulWorkload) {
		t.Fatalf("a stateful workload restart must be refused even if allowlisted, got %v", err)
	}
}

// restart-container: the second reversible class resolves an allowlisted docker container to the EXACT
// [docker restart <container>] argv (built from the shared opschema), records an action_id-bound rollback,
// and enforces the same fail-closed order as restart-service in the container vocabulary.
func TestRestartContainerResolvesAndGuards(t *testing.T) {
	f := &fakeRunner{}
	m := New("web01", "svc-agent", f, WithMutation(safety.NewActuatingChokepoint(), []string{"nginx"}, []string{"librespeed", "app1"}))

	// allowlisted container → exact argv + bound rollback
	_, log, err := m.Mutate(context.Background(), "act-c1", OpClassRestartContainer, "librespeed")
	if err != nil {
		t.Fatalf("an allowlisted container must resolve and run: %v", err)
	}
	if f.argv[len(f.argv)-1] != `'docker' 'restart' 'librespeed'` {
		t.Fatalf("remote command must be the exact POSIX-quoted docker argv, got %q", f.argv[len(f.argv)-1])
	}
	if log.ActionID != "act-c1" || len(log.Rollback) == 0 {
		t.Fatalf("execution_log must bind the rollback to the action id: %+v", log)
	}

	// a (non-stateful) container NOT on the allowlist is refused
	if _, _, err := m.resolveOp(OpClassRestartContainer, "app99"); !errors.Is(err, ErrContainerNotAllowed) {
		t.Fatalf("a non-allowlisted container must be refused, got %v", err)
	}
	// an injection-shaped name never reaches transport
	for _, bad := range []string{"lib; rm -rf /", "a b", "../x", "a/b", "x\ny"} {
		if _, _, err := m.resolveOp(OpClassRestartContainer, bad); !errors.Is(err, ErrInvalidContainer) && !errors.Is(err, ErrContainerNotAllowed) {
			t.Fatalf("an invalid container name %q must be refused (invalid/not-allowed), got %v", bad, err)
		}
	}
	// classifyArgv recognizes the docker restart shape
	if oc, c, ok := classifyArgv([]string{"docker", "restart", "librespeed"}); !ok || oc != OpClassRestartContainer || c != "librespeed" {
		t.Fatalf("classifyArgv must recognize docker restart, got oc=%q c=%q ok=%v", oc, c, ok)
	}
}

// reload-service: the third reversible class resolves an allowlisted unit to [systemctl reload <unit>],
// reusing the SAME unit validation + allowed-units allowlist as restart-service, and is recognized by
// classifyArgv. A non-allowlisted or injection-shaped unit is refused; the floor still applies.
func TestReloadServiceResolvesAndGuards(t *testing.T) {
	f := &fakeRunner{}
	m := New("web01", "svc-agent", f, WithMutation(safety.NewActuatingChokepoint(), []string{"nginx"}, nil))

	_, log, err := m.Mutate(context.Background(), "act-r1", OpClassReloadService, "nginx")
	if err != nil {
		t.Fatalf("an allowlisted unit must reload: %v", err)
	}
	if f.argv[len(f.argv)-1] != `'systemctl' 'reload' 'nginx'` {
		t.Fatalf("remote command must be the exact POSIX-quoted reload argv, got %q", f.argv[len(f.argv)-1])
	}
	if log.ActionID != "act-r1" || len(log.Rollback) == 0 {
		t.Fatalf("execution_log must bind the rollback to the action id: %+v", log)
	}
	// a unit not on the allowed-units allowlist is refused (shared allowlist with restart-service)
	if _, _, err := m.resolveOp(OpClassReloadService, "sshd"); !errors.Is(err, ErrUnitNotAllowed) {
		t.Fatalf("a non-allowlisted unit must be refused, got %v", err)
	}
	// an injection-shaped unit never reaches transport
	if _, _, err := m.resolveOp(OpClassReloadService, "nginx; rm -rf /"); !errors.Is(err, ErrInvalidUnit) {
		t.Fatalf("an invalid unit must be refused, got %v", err)
	}
	// classifyArgv recognizes systemctl reload and does NOT collide with systemctl restart
	if oc, u, ok := classifyArgv([]string{"systemctl", "reload", "nginx"}); !ok || oc != OpClassReloadService || u != "nginx" {
		t.Fatalf("classifyArgv must recognize systemctl reload, got oc=%q u=%q ok=%v", oc, u, ok)
	}
	if oc, _, _ := classifyArgv([]string{"systemctl", "restart", "nginx"}); oc != OpClassRestartService {
		t.Fatalf("reload and restart classifications must stay disjoint, got %q for restart", oc)
	}
}

// start-service: the down-service reversible class resolves an allowlisted unit to [systemctl start <unit>],
// reusing the SAME unit validation + allowed-units allowlist as restart/reload-service, is recognized by
// classifyArgv, and — INV-07 — pairs the forward start argv with the inverse [systemctl stop <unit>] rollback
// (NOT a re-run of the forward start). A non-allowlisted or injection-shaped unit is refused; the floor applies.
func TestStartServiceResolvesAndGuards(t *testing.T) {
	f := &fakeRunner{}
	m := New("web01", "svc-agent", f, WithMutation(safety.NewActuatingChokepoint(), []string{"nginx"}, nil))

	_, log, err := m.Mutate(context.Background(), "act-s1", OpClassStartService, "nginx")
	if err != nil {
		t.Fatalf("an allowlisted unit must start: %v", err)
	}
	if f.argv[len(f.argv)-1] != `'systemctl' 'start' 'nginx'` {
		t.Fatalf("remote command must be the exact POSIX-quoted start argv, got %q", f.argv[len(f.argv)-1])
	}
	// the rollback is the inverse stop argv (carried in the wrapped ssh command), NOT a re-run of the forward start
	if log.ActionID != "act-s1" || len(log.Rollback) == 0 {
		t.Fatalf("execution_log must bind a rollback to the action id: %+v", log)
	}
	if got := log.Rollback[len(log.Rollback)-1]; got != `'systemctl' 'stop' 'nginx'` {
		t.Fatalf("rollback remote command must be the exact POSIX-quoted stop argv, got %q", got)
	}
	// a unit not on the allowed-units allowlist is refused (shared allowlist with restart/reload-service)
	if _, _, err := m.resolveOp(OpClassStartService, "sshd"); !errors.Is(err, ErrUnitNotAllowed) {
		t.Fatalf("a non-allowlisted unit must be refused, got %v", err)
	}
	// an injection-shaped unit never reaches transport
	if _, _, err := m.resolveOp(OpClassStartService, "nginx; rm -rf /"); !errors.Is(err, ErrInvalidUnit) {
		t.Fatalf("an invalid unit must be refused, got %v", err)
	}
	// classifyArgv recognizes systemctl start and stays disjoint from restart/reload
	if oc, u, ok := classifyArgv([]string{"systemctl", "start", "nginx"}); !ok || oc != OpClassStartService || u != "nginx" {
		t.Fatalf("classifyArgv must recognize systemctl start, got oc=%q u=%q ok=%v", oc, u, ok)
	}
	if oc, _, _ := classifyArgv([]string{"systemctl", "restart", "nginx"}); oc != OpClassRestartService {
		t.Fatalf("start and restart classifications must stay disjoint, got %q for restart", oc)
	}
}

// A stateful container (a DB/queue/store) restart is floored even if the name is allowlisted — the same
// data/quorum-loss hard floor restart-service has, keyed on the container name.
func TestRestartContainerStatefulFloored(t *testing.T) {
	m := New("db01", "svc-agent", &fakeRunner{}, WithMutation(safety.NewActuatingChokepoint(), nil, []string{"postgres", "redis"}))
	for _, c := range []string{"postgres", "redis"} {
		if _, _, err := m.resolveOp(OpClassRestartContainer, c); !errors.Is(err, ErrStatefulWorkload) {
			t.Fatalf("a stateful container %q restart must be floored even if allowlisted, got %v", c, err)
		}
	}
}

// A unit that is not on the operator-declared allowed-units allowlist is refused at every entry — resolve,
// Mutate, and the interceptor chokepoint Exec — and never reaches the runner.
func TestUnAllowlistedUnitRefused(t *testing.T) {
	f := &fakeRunner{}
	m := New("web01", "svc-agent", f, WithMutation(safety.NewActuatingChokepoint(), []string{"nginx"}, nil))

	if _, _, err := m.resolveOp(OpClassRestartService, "apache2"); !errors.Is(err, ErrUnitNotAllowed) {
		t.Fatalf("resolveOp must refuse an un-allowlisted unit, got %v", err)
	}
	if _, _, err := m.Mutate(context.Background(), "act-1", OpClassRestartService, "apache2"); !errors.Is(err, ErrUnitNotAllowed) {
		t.Fatalf("Mutate must refuse an un-allowlisted unit, got %v", err)
	}
	if _, err := m.Exec(context.Background(), []string{"systemctl", "restart", "apache2"}, nil); !errors.Is(err, ErrUnitNotAllowed) {
		t.Fatalf("Exec must refuse an un-allowlisted unit, got %v", err)
	}
	if f.argv != nil {
		t.Fatalf("an un-allowlisted unit must never reach the runner, got %v", f.argv)
	}
}

// A fully-admissible canary WITH a test-enabled gate + allowlist + bound evidence executes THROUGH the
// interceptor (single chokepoint), writes a mechanical verdict, audits the decision, and its rollback binds
// to the executed action id (INV-07).
func TestCanaryExecutesThroughInterceptorRecordsAuditAndRollback(t *testing.T) {
	f := &fakeRunner{}
	g := safety.NewActuatingChokepoint()
	m := New("web01", "svc-agent", f, WithMutation(g, []string{"nginx"}, nil))
	ledger := audit.NewLedger()
	i := actuate.NewInterceptor(g, m, ledger)

	out, err := i.Do(context.Background(), canaryRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || out.Refused {
		t.Fatalf("a fully-admissible canary must execute through the chain: %+v", out)
	}
	if !safety.ValidVerdict(out.Verdict) {
		t.Fatalf("a mechanical verdict must be written, got %q", out.Verdict)
	}
	if ledger.Len() == 0 || ledger.Verify() != nil {
		t.Fatalf("the governed decision must be audited to a verifiable ledger, len=%d", ledger.Len())
	}
	if f.argv == nil || f.argv[len(f.argv)-1] != `'systemctl' 'restart' 'nginx'` {
		t.Fatalf("the canary must reach the runner as the quoted argv, got %v", f.argv)
	}
	// the execution_log rollback binds to the SAME action id the interceptor authorized.
	log, err := m.RecordExec(out.ActionID, []string{"systemctl", "restart", "nginx"}, []string{"systemctl", "restart", "nginx"})
	if err != nil {
		t.Fatal(err)
	}
	if log.ActionID != out.ActionID || len(log.Rollback) == 0 {
		t.Fatalf("the execution_log rollback must bind to the executed action id: %+v", log)
	}
}

// BUILD-5: the interceptor's Do records ONE execution_log bound to the executed action id (INV-07) via the
// effect leaf's ExecLog recorder hook — but ONLY on an execution. Gate OFF ⇒ Do refuses before execute ⇒
// nothing runs ⇒ nothing is recorded. Gate ON ⇒ the canary executes and its execution_log is durably
// recorded to the tamper-evident ledger, bound to the exact action id the chain authorized.
func TestInterceptorRecordsExecutionLogThroughDo(t *testing.T) {
	// gate OFF: the canary is refused before execute — no execution_log is recorded.
	gOff := safety.NewReadOnlyChokepoint()
	mOff := New("web01", "svc-agent", &fakeRunner{}, WithMutation(gOff, []string{"nginx"}, nil))
	ledOff := audit.NewLedger()
	iOff := actuate.NewInterceptor(gOff, mOff, ledOff)
	if out, _ := iOff.Do(context.Background(), canaryRequest(t)); !out.Refused {
		t.Fatalf("gate OFF must refuse the canary: %+v", out)
	}
	for _, e := range ledOff.Entries() {
		if strings.Contains(e.Decision, "exec-log") {
			t.Fatalf("gate OFF: nothing executed, so NO execution_log may be recorded, got %q", e.Decision)
		}
	}

	// gate ON: the canary executes through the chain and its execution_log is recorded, bound to the action id.
	f := &fakeRunner{}
	g := safety.NewActuatingChokepoint()
	m := New("web01", "svc-agent", f, WithMutation(g, []string{"nginx"}, nil))
	led := audit.NewLedger()
	i := actuate.NewInterceptor(g, m, led)
	out, err := i.Do(context.Background(), canaryRequest(t))
	if err != nil || !out.Executed {
		t.Fatalf("the canary must execute through the chain: %+v err=%v", out, err)
	}
	found := false
	for _, e := range led.Entries() {
		if strings.Contains(e.Decision, "exec-log") {
			if e.ActionID != out.ActionID {
				t.Fatalf("the execution_log must bind the executed action id, got %q want %q", e.ActionID, out.ActionID)
			}
			if !strings.Contains(e.Reason, "systemctl") || !strings.Contains(e.Reason, "restart") {
				t.Fatalf("the execution_log must carry the bound forward+rollback argv, got %q", e.Reason)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("a gated execution must record an execution_log bound to the action id (INV-07)")
	}
}

// ExecLog derives nothing for a read-only module or a non-mutating argv (there is nothing to record), and
// derives the action_id-bound forward+inverse for the recognized canary restart.
func TestExecLogDerivation(t *testing.T) {
	// read-only module (no gate) ⇒ no log.
	if fwd, _, err := New("web01", "svc", &fakeRunner{}).ExecLog("act-1", []string{"systemctl", "restart", "nginx"}); err != nil || fwd != nil {
		t.Fatalf("a read-only module must derive no execution_log, got fwd=%v err=%v", fwd, err)
	}
	m := New("web01", "svc", &fakeRunner{}, WithMutation(safety.NewReadOnlyChokepoint(), []string{"nginx"}, nil))
	// an unrecognized (non-mutating) argv ⇒ no log.
	if fwd, _, err := m.ExecLog("act-1", []string{"uptime"}); err != nil || fwd != nil {
		t.Fatalf("a non-mutating argv must derive no execution_log, got fwd=%v err=%v", fwd, err)
	}
	// the canary restart ⇒ a bound forward + inverse (the compensating re-restart), wrapped in the ssh invocation.
	fwd, rb, err := m.ExecLog("act-1", []string{"systemctl", "restart", "nginx"})
	if err != nil || len(fwd) == 0 || len(rb) == 0 {
		t.Fatalf("the canary must derive a bound execution_log: fwd=%v rb=%v err=%v", fwd, rb, err)
	}
	if fwd[0] != "ssh" || fwd[len(fwd)-1] != `'systemctl' 'restart' 'nginx'` {
		t.Fatalf("the execution_log forward must be the ssh-wrapped canary argv, got %v", fwd)
	}
}

// Adversarial: an op_class or unit carrying shell metacharacters, spaces, or newlines can neither escape the
// allowlist nor inject on the far host. Even were a metacharacter argument to reach transport, each remote
// argument is POSIX-quoted so it is inert data (INV-02).
func TestAdversarialOpClassAndUnitCannotInject(t *testing.T) {
	f := &fakeRunner{}
	m := New("web01", "svc-agent", f, WithMutation(safety.NewActuatingChokepoint(), []string{"nginx"}, nil))

	for _, unit := range []string{"nginx; rm -rf /", "nginx $(id)", "nginx\nreboot", "a/b", "nginx|cat", "nginx&", "`id`", "ng nx"} {
		if _, _, err := m.resolveOp(OpClassRestartService, unit); !errors.Is(err, ErrInvalidUnit) {
			t.Fatalf("an injecting unit %q must be rejected as an invalid token, got %v", unit, err)
		}
	}
	for _, oc := range []string{"restart-service; evil", "restart-service|x", "restart-service\nreboot"} {
		if _, _, err := m.resolveOp(oc, "nginx"); err == nil {
			t.Fatalf("an injecting op_class %q must have no execution path", oc)
		}
	}
	// an argv with an extra argument cannot be smuggled past the exact-match guard.
	if err := m.guardMutatingArgv([]string{"systemctl", "restart", "nginx", "; reboot"}); !errors.Is(err, ErrNoExecutionPath) {
		t.Fatalf("an argv with an extra argument must be refused, got %v", err)
	}
	// a non-canary verb has no execution path even on an allowlisted unit.
	if err := m.guardMutatingArgv([]string{"systemctl", "stop", "nginx"}); !errors.Is(err, ErrNoExecutionPath) {
		t.Fatalf("a non-canary verb must be refused, got %v", err)
	}
	// belt-and-suspenders: even if a metachar unit reached transport it is POSIX-quoted inert.
	if got := remoteCommand([]string{"systemctl", "restart", "nginx; rm -rf /"}); got != `'systemctl' 'restart' 'nginx; rm -rf /'` {
		t.Fatalf("remote args must be POSIX-quoted so metacharacters are inert, got %q", got)
	}
	if f.argv != nil {
		t.Fatalf("no adversarial input may reach the runner, got %v", f.argv)
	}
}
