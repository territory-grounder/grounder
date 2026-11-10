package faultinjector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TG-624 G3: device-down on a VM. A container answers `pct config`; a VM answers only `qm config`. These
// oracles pin that the kind is read from the owning node, drives every CLI call of the fault's lifecycle
// (stop, timer, reconcile repair, verify), and survives in the ledger row so a reconcile after a restart
// still restarts a VM with `qm`.

// vmKindRunner plays one Proxmox node holding a single guest of a chosen kind. It answers the config and
// status reads the way the real CLIs do (the wrong CLI exits 2, "does not exist") and records every call.
type vmKindRunner struct {
	kind    GuestKind
	name    string
	running bool
	calls   []string
}

func (r *vmKindRunner) Run(_ context.Context, _ string, argv []string) (string, int, error) {
	r.calls = append(r.calls, strings.Join(argv, " "))
	if len(argv) < 2 {
		return "", 0, nil
	}
	owns := argv[0] == r.kind.cli()
	switch argv[1] {
	case "config":
		if !owns {
			return "", 2, nil
		}
		if r.kind == KindQemu {
			return "boot: order=scsi0\nmemory: 1024\nname: " + r.name + "\n", 0, nil
		}
		return "arch: amd64\nhostname: " + r.name + "\n", 0, nil
	case "status":
		if !owns {
			return "", 2, nil
		}
		if r.running {
			return "status: running\n", 0, nil
		}
		return "status: stopped\n", 0, nil
	case "stop":
		if owns {
			r.running = false
			return "", 0, nil
		}
		return "", 2, nil
	case "start":
		if owns {
			r.running = true
			return "", 0, nil
		}
		return "", 2, nil
	}
	return "", 0, nil
}

func (r *vmKindRunner) ran(cmd string) bool {
	for _, c := range r.calls {
		if c == cmd {
			return true
		}
	}
	return false
}

// vmKindStore captures the fault_ref RecordInjection was given and nothing else.
type vmKindStore struct {
	faultRef string
	restored []int64
}

func (s *vmKindStore) RecordInjection(_ context.Context, _ string, _ Class, _, faultRef string, _ time.Duration, _ string) (int64, error) {
	s.faultRef = faultRef
	return 11, nil
}
func (s *vmKindStore) MarkRestored(_ context.Context, id int64) error {
	s.restored = append(s.restored, id)
	return nil
}
func (s *vmKindStore) MarkRestoreFailed(context.Context, int64, string) error { return nil }
func (s *vmKindStore) Outstanding(context.Context) ([]Outstanding, error)     { return nil, nil }
func (s *vmKindStore) RecentRestores(context.Context, time.Duration) (map[string]time.Time, error) {
	return nil, nil
}
func (s *vmKindStore) BreakerOpen(context.Context) (bool, error) { return false, nil }
func (s *vmKindStore) KillSwitchEngaged(context.Context) bool    { return false }

func TestAssertGuest_AVMIsNamedByQmConfig(t *testing.T) {
	r := &vmKindRunner{kind: KindQemu, name: "site01soak01", running: true}
	kind, err := AssertGuest(context.Background(), r, "node-a", "4242", "site01soak01")
	if err != nil {
		t.Fatalf("a VM whose qm config names it must assert: %v", err)
	}
	if kind != KindQemu {
		t.Fatalf("kind = %q, want %q", kind, KindQemu)
	}
	if !r.ran("pct config 4242") || !r.ran("qm config 4242") {
		t.Fatalf("want pct config tried first, then qm config; calls=%v", r.calls)
	}
}

func TestAssertGuest_AContainerNeverAsksQm(t *testing.T) {
	r := &vmKindRunner{kind: KindLXC, name: "site01app01", running: true}
	kind, err := AssertGuest(context.Background(), r, "node-a", "4243", "site01app01")
	if err != nil || kind != KindLXC {
		t.Fatalf("a container must assert as lxc: kind=%q err=%v", kind, err)
	}
	if r.ran("qm config 4243") {
		t.Fatal("a container's assert must not fall through to qm config: its call sequence is unchanged")
	}
}

// The fallback widens what can be ASSERTED, never what can be acted on blind: a VM by another name is still a
// stale pool entry, and a vmid neither CLI knows is refused.
func TestAssertGuest_AVMByAnotherNameAborts(t *testing.T) {
	r := &vmKindRunner{kind: KindQemu, name: "someone-else", running: true}
	if _, err := AssertGuest(context.Background(), r, "node-a", "4242", "site01soak01"); err == nil || !strings.Contains(err.Error(), "SAFETY ABORT") {
		t.Fatalf("a VM with the wrong name must abort as a stale entry, got %v", err)
	}
	f := newFake()
	f.code["pct"], f.code["qm"] = 2, 2
	if _, err := AssertGuest(context.Background(), f, "node-a", "999", "site01soak01"); err == nil {
		t.Fatal("a vmid neither pct nor qm knows must be refused")
	}
}

// The whole lifecycle of a VM device-down: stopped with qm, the ledger row says it is a VM, the timer
// restarts it with qm, and the reconcile path built from the ROW ALONE repairs and verifies it with qm.
func TestInjectGuest_AVMIsStoppedRecordedAndRestoredWithQm(t *testing.T) {
	r := &vmKindRunner{kind: KindQemu, name: "site01soak01", running: true}
	st := &vmKindStore{}
	e := &Engine{Store: st, Exec: r, Limits: Limits{RestoreAfter: 45 * time.Minute}, Note: "test", Log: func(string, ...any) {}}

	ran, err := e.InjectGuest(context.Background(), PoolGuest{VMID: "4242", Name: "site01soak01", Node: "node-a"}, ClassDeviceDown)
	if !ran || err != nil {
		t.Fatalf("inject: ran=%v err=%v", ran, err)
	}
	if !r.ran("qm stop 4242") || r.ran("pct stop 4242") {
		t.Fatalf("a VM must be stopped with qm stop; calls=%v", r.calls)
	}
	if st.faultRef != "qemu:4242" {
		t.Fatalf("ledger fault_ref = %q, want %q (the row must carry the kind)", st.faultRef, "qemu:4242")
	}
	armed := false
	for _, c := range r.calls {
		if strings.HasPrefix(c, "systemd-run ") && strings.HasSuffix(c, " qm start 4242") {
			armed = true
		}
	}
	if !armed {
		t.Fatalf("the deferred restore must arm `qm start`; calls=%v", r.calls)
	}

	// A reconcile after a restart has only the row.
	o := Outstanding{ID: 11, Host: "site01soak01", Class: ClassDeviceDown, Node: "node-a", FaultRef: st.faultRef}
	argv, target, err := UndoArgv(o)
	if err != nil || strings.Join(argv, " ") != "qm start 4242" || target != "node-a" {
		t.Fatalf("undo = %v on %q (err %v), want `qm start 4242` on node-a", argv, target, err)
	}
	r.running = false
	if _, _, err := r.Run(context.Background(), target, argv); err != nil {
		t.Fatal(err)
	}
	up, err := VerifyGuestRunning(context.Background(), r, o.Node, o.FaultRef)
	if err != nil || !up {
		t.Fatalf("a restarted VM must verify running through qm status: up=%v err=%v", up, err)
	}
	if !r.ran("qm status 4242") {
		t.Fatalf("verify must read qm status for a VM; calls=%v", r.calls)
	}
}

// Every row written before TG-624 is a container with a bare-vmid ref; those must keep restoring with pct.
func TestDeviceRef_ABareVmidIsStillAContainer(t *testing.T) {
	if k, v := parseDeviceRef("101101012"); k != KindLXC || v != "101101012" {
		t.Fatalf("bare ref parsed as %q/%q, want lxc/101101012", k, v)
	}
	if k, v := parseDeviceRef(deviceRef(KindQemu, "777")); k != KindQemu || v != "777" {
		t.Fatalf("round trip parsed as %q/%q, want qemu/777", k, v)
	}
	argv, _, err := UndoArgv(Outstanding{ID: 1, Class: ClassDeviceDown, Node: "n", FaultRef: "101"})
	if err != nil || strings.Join(argv, " ") != "pct start 101" {
		t.Fatalf("a pre-TG-624 row must still undo with pct start, got %v (%v)", argv, err)
	}
}

// ssh reports its OWN failure as exit 255 with no Go error, so an unreachable node reads as "pct config
// exited 255" and falls through to qm. That must stay a refusal whatever qm then says: 255 again, or a
// transport error. Neither can produce a kind, so nothing is acted on.
func TestAssertGuest_AnUnreachableNodeIsRefusedNotReclassified(t *testing.T) {
	f := newFake()
	f.code["pct"], f.code["qm"] = 255, 255
	if _, err := AssertGuest(context.Background(), f, "node-a", "4242", "site01soak01"); err == nil {
		t.Fatal("ssh 255 on both reads must be refused")
	}
	tr := &transportFailRunner{}
	if _, err := AssertGuest(context.Background(), tr, "node-a", "4242", "site01soak01"); err == nil {
		t.Fatal("a transport error on the qm read must be refused")
	}
}

// transportFailRunner exits 255 on pct and fails the transport on qm.
type transportFailRunner struct{}

func (transportFailRunner) Run(_ context.Context, _ string, argv []string) (string, int, error) {
	if len(argv) > 0 && argv[0] == "qm" {
		return "", -1, errTransportForKindTest
	}
	return "", 255, nil
}

var errTransportForKindTest = errors.New("ssh: connection reset")

// vmReconcileStore hands the reconciler one overdue VM obligation, as a restarted injector would read it.
type vmReconcileStore struct {
	vmKindStore
	row Outstanding
}

func (s *vmReconcileStore) Outstanding(context.Context) ([]Outstanding, error) {
	return []Outstanding{s.row}, nil
}

// The reconciler, driven from the ledger row alone, must restart a VM with qm start, verify it with qm
// status, and only then discharge the obligation. A reconcile that ran pct would strand the VM and keep the
// host quarantined forever.
func TestReconcileOnce_RestoresAVMFromItsRowWithQm(t *testing.T) {
	r := &vmKindRunner{kind: KindQemu, name: "site01soak01", running: false}
	st := &vmReconcileStore{row: Outstanding{ID: 21, Host: "site01soak01", Class: ClassDeviceDown, Node: "node-a",
		FaultRef: "qemu:4242", RestoreDueAt: time.Now().Add(-time.Minute)}}
	e := &Engine{Store: st, Exec: r, Pool: []PoolGuest{{VMID: "4242", Name: "site01soak01", Node: "node-a"}},
		Log: func(string, ...any) {}}

	if n := e.ReconcileOnce(context.Background()); n != 1 {
		t.Fatalf("closed %d obligations, want 1; calls=%v", n, r.calls)
	}
	if !r.ran("qm start 4242") || !r.ran("qm status 4242") || r.ran("pct start 4242") {
		t.Fatalf("the reconcile must start and verify the VM with qm; calls=%v", r.calls)
	}
	if len(st.restored) != 1 || st.restored[0] != 21 {
		t.Fatalf("restored = %v, want [21]", st.restored)
	}
}

// probeFor must find a VM's declared app probe by its vmid even after a rename (the name fallback misses).
func TestProbeFor_FindsAVMByTheVmidInsideItsRef(t *testing.T) {
	e := &Engine{Pool: []PoolGuest{{VMID: "4242", Name: "site01soak01", HealthProbe: "curl -sf http://127.0.0.1/"}}}
	got := e.probeFor(Outstanding{Class: ClassDeviceDown, Host: "renamed-meanwhile", FaultRef: "qemu:4242"})
	if got == "" {
		t.Fatal("a VM obligation must find its guest's probe by vmid, not lose it to a rename")
	}
}
