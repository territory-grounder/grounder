package faultinjector

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeRunner records argv and replays scripted results, so the safety properties can be asserted without
// touching a machine.
type fakeRunner struct {
	calls  [][]string
	hosts  []string
	stdout map[string]string // first argv element -> stdout
	code   map[string]int
}

func (f *fakeRunner) Run(_ context.Context, host string, argv []string) (string, int, error) {
	f.calls = append(f.calls, argv)
	f.hosts = append(f.hosts, host)
	key := ""
	if len(argv) > 0 {
		key = argv[0]
	}
	return f.stdout[key], f.code[key], nil
}

func newFake() *fakeRunner {
	return &fakeRunner{stdout: map[string]string{}, code: map[string]int{}}
}

// ---------------------------------------------------------------------------------------------------
// Name-assert: I once stopped the WRONG guest from a stale vmid. Any doubt must abort.
// ---------------------------------------------------------------------------------------------------

func TestAssertGuestName_AcceptsAMatch(t *testing.T) {
	f := newFake()
	f.stdout["pct"] = "arch: amd64\nhostname: dc1mealie01\nmemory: 2048\n"
	if err := AssertGuestName(context.Background(), f, "pve01", "101", "dc1mealie01"); err != nil {
		t.Fatalf("want match accepted, got %v", err)
	}
}

func TestAssertGuestName_AbortsOnMismatch(t *testing.T) {
	f := newFake()
	f.stdout["pct"] = "hostname: dc1SOMETHINGELSE\n"
	err := AssertGuestName(context.Background(), f, "pve01", "101", "dc1mealie01")
	if err == nil {
		t.Fatal("a stale pool entry was accepted — this is how the wrong guest gets stopped")
	}
	if !strings.Contains(err.Error(), "SAFETY ABORT") {
		t.Fatalf("want a SAFETY ABORT, got %v", err)
	}
}

// An empty/unparseable hostname means we do not know what the machine is. Proceeding would be acting blind.
func TestAssertGuestName_AbortsWhenHostnameIsAbsent(t *testing.T) {
	f := newFake()
	f.stdout["pct"] = "arch: amd64\nmemory: 2048\n" // no hostname line
	if err := AssertGuestName(context.Background(), f, "pve01", "101", "dc1mealie01"); err == nil {
		t.Fatal("accepted a config with no hostname — must refuse to act blind")
	}
}

// A non-zero pct exit (unreachable node, no such vmid) must abort, not fall through to the action.
func TestAssertGuestName_AbortsOnNonZeroExit(t *testing.T) {
	f := newFake()
	f.code["pct"] = 2
	if err := AssertGuestName(context.Background(), f, "pve01", "999", "dc1mealie01"); err == nil {
		t.Fatal("accepted a failed pct config — must abort")
	}
}

// ---------------------------------------------------------------------------------------------------
// Restore-unit naming: the collision that stranded docuseal01.
// ---------------------------------------------------------------------------------------------------

// The bash engine used a FIXED unit name per vmid for the disk arm. A fixed name collides with a lingering
// unit from a previous injection and the arm silently fails. Names must be unique per injection.
func TestRestoreUnitName_IsUniquePerInjection(t *testing.T) {
	t0 := time.Unix(1785000000, 0).UTC()
	a := restoreUnitName(ClassDiskFill, "101", t0)
	b := restoreUnitName(ClassDiskFill, "101", t0.Add(time.Second))
	if a == b {
		t.Fatalf("two injections for one vmid produced the same unit name %q — a collision makes the arm silently fail", a)
	}
}

// Every class gets the unique treatment, not just device-down. The disk arm lacking it was half the stranding.
func TestRestoreUnitName_DistinguishesClasses(t *testing.T) {
	t0 := time.Unix(1785000000, 0).UTC()
	if restoreUnitName(ClassDiskFill, "101", t0) == restoreUnitName(ClassDeviceDown, "101", t0) {
		t.Fatal("disk-fill and device-down restores share a unit name — one would clobber the other")
	}
}

// --collect is what stops a fired unit lingering and guaranteeing the NEXT collision.
func TestArmDeferredRestore_UsesCollectAndAUniqueUnit(t *testing.T) {
	f := newFake()
	err := ArmDeferredRestore(context.Background(), f, "pve01", ClassDeviceDown, "101", 30*time.Minute,
		[]string{"pct", "start", "101"})
	if err != nil {
		t.Fatalf("arm failed: %v", err)
	}

	var armed []string
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == "systemd-run" {
			armed = c
		}
	}
	if armed == nil {
		t.Fatal("no systemd-run call was made")
	}
	joined := strings.Join(armed, " ")
	if !strings.Contains(joined, "--collect") {
		t.Errorf("missing --collect: a fired unit lingers and guarantees the next collision: %v", armed)
	}
	if !strings.Contains(joined, "--on-active=30min") {
		t.Errorf("wrong delay: %v", armed)
	}
	if !strings.Contains(joined, "--unit=tg-restore-") {
		t.Errorf("missing unique unit name: %v", armed)
	}
	// The undo argv must be passed through intact and unquoted-into-a-shell.
	if !strings.HasSuffix(joined, "pct start 101") {
		t.Errorf("undo argv not appended verbatim: %v", armed)
	}
}

// A stale fixed-name unit from the OLD engine must be cleared before arming, or the arm fails.
func TestArmDeferredRestore_ResetsAStaleFailedUnitFirst(t *testing.T) {
	f := newFake()
	_ = ArmDeferredRestore(context.Background(), f, "pve01", ClassDiskFill, "101", 30*time.Minute, []string{"rm", "-f", "/x"})
	if len(f.calls) == 0 || f.calls[0][0] != "systemctl" || f.calls[0][1] != "reset-failed" {
		t.Fatalf("want reset-failed before arming, got %v", f.calls)
	}
}

// ---------------------------------------------------------------------------------------------------
// Undo argv
// ---------------------------------------------------------------------------------------------------

// The repair must run on the host that SURVIVES the fault: a device-down undo on the Proxmox node (the guest
// is off), a disk-fill undo inside the guest (which is up).
func TestUndoArgv_TargetsTheSurvivingHost(t *testing.T) {
	dev := Outstanding{ID: 1, Host: "alpha", Class: ClassDeviceDown, Node: "pve01", FaultRef: "101"}
	argv, target, err := UndoArgv(dev)
	if err != nil {
		t.Fatalf("device-down undo: %v", err)
	}
	if target != "pve01" {
		t.Errorf("device-down undo must run on the owning NODE (the guest is off), got %q", target)
	}
	if strings.Join(argv, " ") != "pct start 101" {
		t.Errorf("argv = %v", argv)
	}

	disk := Outstanding{ID: 2, Host: "alpha", Class: ClassDiskFill, Node: "pve01", FaultRef: "/var/tmp/f.img"}
	argv, target, err = UndoArgv(disk)
	if err != nil {
		t.Fatalf("disk-fill undo: %v", err)
	}
	if target != "alpha" {
		t.Errorf("disk-fill undo must run inside the guest, got %q", target)
	}
	if strings.Join(argv, " ") != "rm -f -- /var/tmp/f.img" {
		t.Errorf("argv = %v — the -- guard keeps a hostile path from becoming a flag", argv)
	}
}

// A disk-fill with no recorded handle cannot be repaired by a fresh process — that is precisely the
// information the ledger exists to carry, so its absence must be a loud error, not a silent skip.
func TestUndoArgv_FailsLoudlyWithoutAFaultRef(t *testing.T) {
	if _, _, err := UndoArgv(Outstanding{ID: 3, Class: ClassDiskFill}); err == nil {
		t.Fatal("a disk-fill with no fault_ref must error, not silently no-op")
	}
}

func TestUndoArgv_RejectsSelfReleasingAndUnknownClasses(t *testing.T) {
	if _, _, err := UndoArgv(Outstanding{ID: 4, Class: ClassMemPressure}); err == nil {
		t.Error("mem-pressure owes no restore and must not produce an undo")
	}
	if _, _, err := UndoArgv(Outstanding{ID: 5, Class: Class("nonsense")}); err == nil {
		t.Error("an unknown class must error rather than produce an argv")
	}
}
