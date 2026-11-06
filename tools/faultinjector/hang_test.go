package faultinjector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TG-624 G3: a STOPPED container or unit is healed with its literal inverse (start-*), so restart-container
// and restart-service had no fault TG would answer with them. The hang classes freeze a declared container
// (docker pause) or unit (SIGSTOP): still "up", serving nothing, healed only by a restart.

// hangRunner plays one guest holding a container and a unit that can each be frozen and thawed.
type hangRunner struct {
	paused, frozen bool
	active         bool
	mainPID        string
	calls          []string
}

func (r *hangRunner) Run(_ context.Context, _ string, argv []string) (string, int, error) {
	c := strings.Join(argv, " ")
	r.calls = append(r.calls, c)
	switch {
	case strings.HasPrefix(c, "pct config"):
		return "hostname: guest-h\n", 0, nil
	case strings.HasPrefix(c, "docker pause "):
		r.paused = true
	case strings.HasPrefix(c, "docker unpause "):
		if !r.paused {
			return "Error: container is not paused", 1, nil
		}
		r.paused = false
	case strings.HasPrefix(c, "docker inspect "):
		if r.paused {
			return "paused\n", 0, nil
		}
		return "running\n", 0, nil
	case c == "systemctl kill --signal=SIGSTOP web":
		r.frozen = true
	case c == "systemctl kill --signal=SIGCONT web":
		r.frozen = false
	case c == "systemctl is-active web":
		if r.active {
			return "active\n", 0, nil
		}
		return "inactive\n", 3, nil
	case c == "systemctl show --property=MainPID --value web":
		return r.mainPID + "\n", 0, nil
	case strings.HasPrefix(c, "ps -o stat= -p "):
		if r.frozen {
			return "Tl\n", 0, nil
		}
		return "Ssl\n", 0, nil
	}
	return "", 0, nil
}

func (r *hangRunner) ran(cmd string) bool {
	for _, c := range r.calls {
		if c == cmd {
			return true
		}
	}
	return false
}

// hangStore keeps the fault_ref and the discharges.
type hangStore struct{ vmKindStore }

func TestHangClasses_ProvokeOnlyTheRestartVerbs(t *testing.T) {
	if got := ClassContainerHang.Provokes(); len(got) != 1 || got[0] != "restart-container" {
		t.Fatalf("container-hang provokes %v, want [restart-container]: start-container on a running container heals nothing", got)
	}
	if got := ClassServiceHang.Provokes(); len(got) != 1 || got[0] != "restart-service" {
		t.Fatalf("service-hang provokes %v, want [restart-service]", got)
	}
	if _, err := ParseClasses("container-hang,service-hang"); err != nil {
		t.Fatalf("the rotation must accept both hang classes: %v", err)
	}
}

func TestContainerHang_PausesRecordsAndArmsUnpause(t *testing.T) {
	r := &hangRunner{}
	st := &hangStore{}
	e := &Engine{Store: st, Exec: r, Limits: Limits{RestoreAfter: 45 * time.Minute}, Note: "t", Log: func(string, ...any) {}}
	ran, err := e.InjectGuest(context.Background(), PoolGuest{VMID: "9", Name: "guest-h", Node: "n", Container: "app"}, ClassContainerHang)
	if !ran || err != nil || !r.paused {
		t.Fatalf("inject: ran=%v err=%v paused=%v calls=%v", ran, err, r.paused, r.calls)
	}
	if st.faultRef != "app" {
		t.Fatalf("fault_ref = %q, want the declared container", st.faultRef)
	}
	armed := false
	for _, c := range r.calls {
		if strings.HasPrefix(c, "systemd-run ") && strings.HasSuffix(c, " docker unpause app") {
			armed = true
		}
	}
	if !armed {
		t.Fatalf("the deferred restore must arm `docker unpause app`; calls=%v", r.calls)
	}
	// A paused container must not verify as repaired; an unpaused one must.
	o := Outstanding{ID: 3, Host: "guest-h", Class: ClassContainerHang, FaultRef: "app"}
	if ok, _ := e.verifyRepaired(context.Background(), o); ok {
		t.Fatal("a still-paused container verified as repaired")
	}
	r.paused = false
	if ok, err := e.verifyRepaired(context.Background(), o); !ok || err != nil {
		t.Fatalf("an unpaused container must verify repaired: ok=%v err=%v", ok, err)
	}
}

// The reconciler thaws a frozen unit with SIGCONT and verifies by the main process's state: is-active reads
// "active" the whole time a unit is frozen, so it alone would discharge a hang that is still in place.
func TestServiceHang_ReconcileThawsAndVerifiesByProcessState(t *testing.T) {
	r := &hangRunner{active: true, mainPID: "4242", frozen: true}
	st := &vmReconcileStore{row: Outstanding{ID: 31, Host: "guest-h", Class: ClassServiceHang, FaultRef: "web",
		RestoreDueAt: time.Now().Add(-time.Minute)}}
	e := &Engine{Store: st, Exec: r, Log: func(string, ...any) {}}

	ok, _ := VerifyServiceThawed(context.Background(), r, "guest-h", "web")
	if ok {
		t.Fatal("a frozen unit (active, main process in T) verified as thawed")
	}
	if n := e.ReconcileOnce(context.Background()); n != 1 {
		t.Fatalf("closed %d, want 1; calls=%v", n, r.calls)
	}
	if !r.ran("systemctl kill --signal=SIGCONT web") || r.frozen {
		t.Fatalf("the reconcile must SIGCONT the unit; calls=%v", r.calls)
	}
}

func TestVerifyServiceThawed_FailsClosedWithoutAMainPID(t *testing.T) {
	r := &hangRunner{active: true, mainPID: "0"}
	if ok, err := VerifyServiceThawed(context.Background(), r, "guest-h", "web"); ok || err == nil {
		t.Fatalf("no main PID must not verify (ok=%v err=%v)", ok, err)
	}
	r2 := &hangRunner{active: false, mainPID: "77"}
	if ok, _ := VerifyServiceThawed(context.Background(), r2, "guest-h", "web"); ok {
		t.Fatal("an inactive unit must not verify as thawed")
	}
}

// Eligibility: a hang class may only freeze what the operator declared.
func TestHangClasses_SkipGuestsWithoutADeclaration(t *testing.T) {
	g := PoolGuest{VMID: "9", Name: "guest-h", Node: "n"} // declares neither a unit nor a container
	st := State{Now: time.Now(), Pool: []PoolGuest{g}, Allowlist: map[string]bool{"guest-h": true},
		Status: map[string]string{"9": "running"}, Limits: Limits{MaxDown: 1, MaxBusy: 3}}
	for _, c := range []Class{ClassContainerHang, ClassServiceHang} {
		if d := PlanNext(st, []Class{c}); d.Act {
			t.Fatalf("%s chose a guest with nothing declared to freeze: %+v", c, d)
		}
	}
}

func TestServiceHang_FreezesRecordsAndArmsSIGCONT(t *testing.T) {
	r := &hangRunner{active: true, mainPID: "4242"}
	st := &hangStore{}
	e := &Engine{Store: st, Exec: r, Limits: Limits{RestoreAfter: 45 * time.Minute}, Note: "t", Log: func(string, ...any) {}}
	ran, err := e.InjectGuest(context.Background(), PoolGuest{VMID: "9", Name: "guest-h", Node: "n", Unit: "web"}, ClassServiceHang)
	if !ran || err != nil || !r.frozen {
		t.Fatalf("inject: ran=%v err=%v frozen=%v calls=%v", ran, err, r.frozen, r.calls)
	}
	if st.faultRef != "web" {
		t.Fatalf("fault_ref = %q, want the declared unit", st.faultRef)
	}
	armed := false
	for _, c := range r.calls {
		if strings.HasPrefix(c, "systemd-run ") && strings.HasSuffix(c, " systemctl kill --signal=SIGCONT web") {
			armed = true
		}
	}
	if !armed {
		t.Fatalf("the deferred restore must arm SIGCONT; calls=%v", r.calls)
	}
}

// A unit with no running main process could never be verified thawed, so it is refused BEFORE any effect
// and the obligation closes (provably nothing was frozen).
func TestServiceHang_RefusesAUnitWithNoMainProcess(t *testing.T) {
	r := &hangRunner{active: true, mainPID: "0"}
	st := &hangStore{}
	e := &Engine{Store: st, Exec: r, Limits: Limits{RestoreAfter: 45 * time.Minute}, Note: "t", Log: func(string, ...any) {}}
	ran, err := e.InjectGuest(context.Background(), PoolGuest{VMID: "9", Name: "guest-h", Node: "n", Unit: "web"}, ClassServiceHang)
	if ran || err == nil || !errors.Is(err, ErrPreEffect) || r.frozen {
		t.Fatalf("want a pre-effect refusal with nothing frozen: ran=%v err=%v frozen=%v", ran, err, r.frozen)
	}
	if len(st.restored) != 1 {
		t.Fatalf("a pre-effect refusal must close its obligation, restored=%v", st.restored)
	}
}

// SIGCONT cannot start a unit. A unit that went inactive while frozen is restored by the reconcile's
// idempotent follow-up start, instead of quarantining the host forever.
func TestServiceHang_ReconcileStartsAUnitThatWentInactive(t *testing.T) {
	r := &startableHangRunner{hangRunner: hangRunner{active: false, mainPID: "0"}}
	st := &vmReconcileStore{row: Outstanding{ID: 32, Host: "guest-h", Class: ClassServiceHang, FaultRef: "web",
		RestoreDueAt: time.Now().Add(-time.Minute)}}
	e := &Engine{Store: st, Exec: r, Log: func(string, ...any) {}}
	if n := e.ReconcileOnce(context.Background()); n != 1 {
		t.Fatalf("closed %d, want 1 (the follow-up start must restore an inactive unit); calls=%v", n, r.calls)
	}
	if !r.ran("systemctl start web") {
		t.Fatalf("want the follow-up `systemctl start web`; calls=%v", r.calls)
	}
}

// startableHangRunner is hangRunner where `systemctl start` brings the unit up with a running main process.
type startableHangRunner struct{ hangRunner }

func (r *startableHangRunner) Run(ctx context.Context, host string, argv []string) (string, int, error) {
	if strings.Join(argv, " ") == "systemctl start web" {
		r.calls = append(r.calls, "systemctl start web")
		r.active, r.mainPID, r.frozen = true, "5150", false
		return "", 0, nil
	}
	return r.hangRunner.Run(ctx, host, argv)
}

// Every class that owes a restore must have an injector wired: an unwired one returns "no injector wired"
// (pre-effect) and is silently never injected.
func TestEveryRestoringClassHasAnInjectorWired(t *testing.T) {
	for _, c := range AllClasses() {
		if !c.OwesRestore() {
			continue
		}
		e := &Engine{Exec: permissiveInjectRunner{}, Log: func(string, ...any) {}}
		d := Decision{Act: true, Class: c, Guest: PoolGuest{VMID: "9", Name: "guest-h", Node: "n", Unit: "web",
			Container: "app", LogPath: "/var/log/tg-logfill/app.log"}}
		if err := e.performInjection(context.Background(), d); err != nil && strings.Contains(err.Error(), "no injector wired") {
			t.Errorf("class %q owes a restore but has no injector wired", c)
		}
	}
}

// permissiveInjectRunner answers every read so each class's injector reaches its effect.
type permissiveInjectRunner struct{}

func (permissiveInjectRunner) Run(_ context.Context, _ string, argv []string) (string, int, error) {
	switch {
	case len(argv) > 0 && argv[0] == "df":
		return "size used\n10737418240 1073741824\n", 0, nil
	case len(argv) > 2 && argv[0] == "systemctl" && argv[1] == "show":
		return "4242\n", 0, nil
	}
	return "", 0, nil
}

func TestValidSubjectName_RefusesShellAndOptionShapes(t *testing.T) {
	for _, ok := range []string{"nginx", "nginx.service", "leantime_leantime_1", "actualbudget-actual_server-1", "getty@tty1"} {
		if !ValidSubjectName(ok) {
			t.Errorf("%q must be accepted", ok)
		}
	}
	for _, bad := range []string{"-x", "a b", "a;id", "$(id)", "a|b", "", "../x"} {
		if ValidSubjectName(bad) {
			t.Errorf("%q must be refused", bad)
		}
	}
	if _, _, err := UndoArgv(Outstanding{ID: 1, Class: ClassServiceHang, Host: "h", FaultRef: "web;reboot"}); err == nil {
		t.Fatal("an unsafe ledger fault_ref must not reach a remote command line")
	}
}
