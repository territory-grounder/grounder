package faultinjector

import (
	"context"
	"strings"
	"testing"
)

// CONTAINER-DOWN — the fault that reaches `restart-container`.
//
// No other class in the rotation exercises that op-class: a device-down stops the whole GUEST (healed by
// start-guest) and a disk-fill never touches a service. This class stops ONE operator-declared container while
// the guest stays UP, so the fault presents as a SERVICE fault on a reachable host.
//
// It stops a real production service, so the safety properties below are the point of the class, not
// decoration: it must never choose its own victim, and its restore must name exactly what it stopped.

// THE CENTRAL SAFETY PROPERTY. The container is operator-declared; an undeclared guest must abort rather than
// pick something. Scraping `docker ps` for a victim could stop a database or the log shipper, and would make
// the fault non-reproducible run to run.
func TestContainerDownRefusesWhenNoContainerIsDeclared(t *testing.T) {
	f := newFake()
	err := InjectContainerDown(context.Background(), f, "dc1mealie01", "")
	if err == nil {
		t.Fatal("an undeclared container must ABORT — never be guessed")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("the refusal must say what it refused to do, got %q", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("nothing may be run on the host when the target is unknown, got %v", f.calls)
	}
}

// The planner enforces the same rule one level up: a guest with no declared container is never SELECTED for
// this class, so the abort above is a backstop rather than the only guard.
func TestPlannerSkipsGuestsWithNoDeclaredContainer(t *testing.T) {
	st := State{
		Pool: []PoolGuest{
			{VMID: "101", Name: "guest-a", Node: "pve01"},                       // no container declared
			{VMID: "102", Name: "guest-b", Node: "pve01", Container: "the-app"}, // eligible
		},
		Allowlist: map[string]bool{"guest-a": true, "guest-b": true},
		Status:    map[string]string{"101": "running", "102": "running"},
		Limits:    Limits{MaxDown: 4, MaxBusy: 7},
	}
	d := PlanNext(st, []Class{ClassContainerDown})
	if !d.Act {
		t.Fatalf("an eligible guest exists, so the planner must act: %s", d.Reason)
	}
	if d.Guest.Name != "guest-b" {
		t.Fatalf("only the guest with a DECLARED container is eligible, got %q", d.Guest.Name)
	}
}

func TestPlannerWontInjectContainerDownWithNoEligibleGuest(t *testing.T) {
	st := State{
		Pool:      []PoolGuest{{VMID: "101", Name: "guest-a", Node: "pve01"}}, // none declared
		Allowlist: map[string]bool{"guest-a": true},
		Status:    map[string]string{"101": "running"},
		Limits:    Limits{MaxDown: 4, MaxBusy: 7},
	}
	if d := PlanNext(st, []Class{ClassContainerDown}); d.Act {
		t.Fatalf("with no declared container anywhere the planner must stand down, got %+v", d.Guest)
	}
}

// The effect is a fixed argv on the GUEST — never on the Proxmox node, which is where a device-down acts.
func TestContainerDownStopsTheDeclaredContainerOnTheGuest(t *testing.T) {
	f := newFake()
	if err := InjectContainerDown(context.Background(), f, "dc1mealie01", "mealie"); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("want exactly one command, got %v", f.calls)
	}
	if got, want := strings.Join(f.calls[0], " "), "docker stop mealie"; got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
	if f.hosts[0] != "dc1mealie01" {
		t.Errorf("a container-down acts on the GUEST, got host %q", f.hosts[0])
	}
}

// A non-zero exit is a FAILED injection, so the caller voids the obligation instead of leaving a phantom that
// quarantines a healthy host forever.
func TestContainerDownSurfacesANonZeroExit(t *testing.T) {
	f := newFake()
	f.code["docker"] = 1
	if err := InjectContainerDown(context.Background(), f, "h", "c"); err == nil {
		t.Fatal("a non-zero docker exit must be reported as a failed injection")
	}
}

// THE RESTORE MUST NAME WHAT WAS STOPPED. fault_ref carries the container, and the undo is its plain inverse
// on the same host. A restore that cannot name its target is an obligation that can never be discharged.
func TestContainerDownRestoreIsTheInverseOnTheSameHost(t *testing.T) {
	argv, host, err := UndoArgv(Outstanding{ID: 7, Class: ClassContainerDown, Host: "dc1mealie01", Node: "pve01", FaultRef: "mealie"})
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if got, want := strings.Join(argv, " "), "docker start mealie"; got != want {
		t.Errorf("undo argv = %q, want %q", got, want)
	}
	// The guest stays UP through a container-down, so the repair runs there — unlike a device-down, whose
	// repair must run on the node because the guest is stopped (the docuseal01 stranding).
	if host != "dc1mealie01" {
		t.Errorf("the repair runs on the GUEST, got %q", host)
	}
}

func TestContainerDownRestoreRefusesWithoutAFaultRef(t *testing.T) {
	if _, _, err := UndoArgv(Outstanding{ID: 8, Class: ClassContainerDown, Host: "h"}); err == nil {
		t.Fatal("an obligation with no fault_ref cannot be discharged — it must fail loud, not guess")
	}
}

// It owes a restore. If this were false the engine would inject and never repair.
func TestContainerDownOwesARestore(t *testing.T) {
	if !ClassContainerDown.OwesRestore() {
		t.Fatal("container-down stops a real service and MUST owe a restore")
	}
}

// The discharge is VERIFIED by reading status, never inferred from `docker start`'s exit code — the same
// discipline device-down applies (REQ-2508: an unverified discharge quarantines rather than assumes).
func TestContainerDownDischargeIsVerifiedByReadingStatus(t *testing.T) {
	f := newFake()
	f.stdout["docker"] = "running\n"
	ok, err := VerifyContainerRunning(context.Background(), f, "h", "c")
	if err != nil || !ok {
		t.Fatalf("a running container must verify as repaired: ok=%v err=%v", ok, err)
	}
	f2 := newFake()
	f2.stdout["docker"] = "exited\n"
	if ok, _ := VerifyContainerRunning(context.Background(), f2, "h", "c"); ok {
		t.Fatal("an exited container must NOT verify as repaired — that is how a fault gets stranded")
	}
}

// MUTATION CONTROL. The suite is only meaningful if the container name is actually threaded rather than
// hardcoded or dropped. Two different declared containers must produce two different argvs, in both the
// inject and the restore direction — if either comes back identical, the name is not being carried.
func TestMutationControl_TheDeclaredContainerNameIsActuallyCarried(t *testing.T) {
	a, b := newFake(), newFake()
	if err := InjectContainerDown(context.Background(), a, "h", "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := InjectContainerDown(context.Background(), b, "h", "beta"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(a.calls[0], " ") == strings.Join(b.calls[0], " ") {
		t.Fatal("two different containers produced the SAME stop argv — the declared name is not being carried")
	}
	ua, _, _ := UndoArgv(Outstanding{Class: ClassContainerDown, Host: "h", FaultRef: "alpha"})
	ub, _, _ := UndoArgv(Outstanding{Class: ClassContainerDown, Host: "h", FaultRef: "beta"})
	if strings.Join(ua, " ") == strings.Join(ub, " ") {
		t.Fatal("two different containers produced the SAME restore argv — a restore would target the wrong one")
	}
}
