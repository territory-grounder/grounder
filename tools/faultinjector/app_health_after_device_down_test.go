package faultinjector

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TG-226. A device-down restore verified `pct status` said "running" and closed the obligation. That
// proves the GUEST is back; it says nothing about whether the applications inside re-established their
// downstream connections after a hard stop.
//
// Found live 2026-07-31 on dc1habitica01: a single device-down fault hard-stopped the guest, MongoDB
// recovered on reboot, and the Node app's Mongoose connection pool came back WEDGED — every DB operation
// buffered to a 10s timeout for ~5 hours. Static and in-memory endpoints returned 200 throughout, so ICMP,
// LibreNMS device-status, `mongo ping` and `pct status` ALL read healthy for the whole outage. Only
// `systemctl restart habitica.service` cleared it.
//
// The property is not "habitica is checked" — naming one guest leaves the next one exposed exactly the
// same way. It is that a device-down restore consults the guest's declared DATA-PATH probe when there is
// one, and SAYS SO when there is not: a check that cannot report "there was nothing to check" reads as
// full coverage while covering nothing.

// appProbeRunner answers pct status and the app probe independently, so a test can express the exact
// habitica state — guest running, application wedged — which is the case every existing verifier calls
// repaired.
type appProbeRunner struct {
	guestRunning bool
	probeExit    int
	probeCalls   int
	probeArgv    []string
}

func (r *appProbeRunner) Run(_ context.Context, _ string, argv []string) (string, int, error) {
	if len(argv) >= 2 && argv[0] == "pct" && argv[1] == "status" {
		if r.guestRunning {
			return "status: running\n", 0, nil
		}
		return "status: stopped\n", 0, nil
	}
	r.probeCalls++
	r.probeArgv = argv
	return "", r.probeExit, nil
}

func appProbeEngine(t *testing.T, r Runner, probe string) (*Engine, *[]string) {
	t.Helper()
	var logged []string
	e := &Engine{
		Exec: r,
		Pool: []PoolGuest{{VMID: "101", Name: "guest-a", Node: "node-1", HealthProbe: probe}},
		Log:  func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) },
	}
	return e, &logged
}

// TestAWedgedAppFailsTheDeviceDownRestore is the finding: the habitica state exactly.
func TestAWedgedAppFailsTheDeviceDownRestore(t *testing.T) {
	r := &appProbeRunner{guestRunning: true, probeExit: 1} // guest back, application wedged
	e, logged := appProbeEngine(t, r, "/usr/local/bin/tg-app-health")

	ok, err := e.verifyRepaired(context.Background(), Outstanding{
		Host: "guest-a", Class: ClassDeviceDown, Node: "node-1", FaultRef: "101",
	})
	if err != nil {
		t.Fatalf("verifyRepaired errored: %v", err)
	}
	if ok {
		t.Fatal("a device-down restore reported REPAIRED while the guest's app-health probe was failing. " +
			"That is the habitica wedge: pct status says running, the connection pool is dead, and the " +
			"obligation closes PERMANENTLY (MarkRestored is never revisited) on a host that is still broken.")
	}
	if r.probeCalls != 1 {
		t.Errorf("the probe ran %d times, want 1 — the verifier is not consulting it at all", r.probeCalls)
	}
	var sawWedge bool
	for _, l := range *logged {
		if strings.Contains(l, "probe FAILED") {
			sawWedge = true
		}
	}
	if !sawWedge {
		t.Errorf("nothing in the log names the wedge, so a quarantined host gives an operator no reason:\n%v",
			*logged)
	}
}

// TestAHealthyAppStillPassesTheRestore is the vacuity floor, and the one the "test the fixed rule on
// today's data" lesson demands: the new check must stay SILENT on a healthy estate, or the first campaign
// quarantines the whole pool.
func TestAHealthyAppStillPassesTheRestore(t *testing.T) {
	r := &appProbeRunner{guestRunning: true, probeExit: 0}
	e, logged := appProbeEngine(t, r, "/usr/local/bin/tg-app-health")

	ok, err := e.verifyRepaired(context.Background(), Outstanding{
		Host: "guest-a", Class: ClassDeviceDown, Node: "node-1", FaultRef: "101",
	})
	if err != nil || !ok {
		t.Fatalf("a healthy guest with a PASSING probe was not accepted as repaired (ok=%v err=%v) — the "+
			"check fires on the healthy case, which would strand every restore in the pool", ok, err)
	}
	for _, l := range *logged {
		if strings.Contains(l, "FAILED") || strings.Contains(l, "GUEST level only") {
			t.Errorf("a fully-verified restore still logged a warning: %q", l)
		}
	}
}

// TestAStoppedGuestNeverReachesTheProbe. The guest check must stay FIRST: probing an app inside a stopped
// guest costs an ssh round-trip per drain tick and reports a failure whose real cause is the guest.
func TestAStoppedGuestNeverReachesTheProbe(t *testing.T) {
	r := &appProbeRunner{guestRunning: false, probeExit: 0}
	e, _ := appProbeEngine(t, r, "/usr/local/bin/tg-app-health")

	ok, err := e.verifyRepaired(context.Background(), Outstanding{
		Host: "guest-a", Class: ClassDeviceDown, Node: "node-1", FaultRef: "101",
	})
	if ok || err != nil {
		t.Fatalf("a STOPPED guest was reported repaired (ok=%v err=%v)", ok, err)
	}
	if r.probeCalls != 0 {
		t.Errorf("the app probe ran %d times against a stopped guest", r.probeCalls)
	}
}

// TestAnUndeclaredProbeIsSaidOutLoud is the "a check that cannot report nothing to check" guard. The probe
// is opt-in per guest, so most guests have none — and a restore verified at guest level only must not be
// reported the same way as one verified through the app.
func TestAnUndeclaredProbeIsSaidOutLoud(t *testing.T) {
	r := &appProbeRunner{guestRunning: true, probeExit: 0}
	e, logged := appProbeEngine(t, r, "") // no probe declared

	ok, err := e.verifyRepaired(context.Background(), Outstanding{
		Host: "guest-a", Class: ClassDeviceDown, Node: "node-1", FaultRef: "101",
	})
	if err != nil || !ok {
		t.Fatalf("an undeclared probe must not fail the restore (ok=%v err=%v) — refusing every undeclared "+
			"guest would strand the whole pool on the first run", ok, err)
	}
	if r.probeCalls != 0 {
		t.Errorf("something ran as a probe for a guest that declares none: %v", r.probeArgv)
	}
	var announced bool
	for _, l := range *logged {
		if strings.Contains(l, "GUEST level only") {
			announced = true
		}
	}
	if !announced {
		t.Errorf("a guest-level-only restore was recorded exactly like a fully-verified one. The absence of "+
			"a probe is the state TG-226 is about; a verifier that cannot say \"there was nothing to check\" "+
			"reads as full coverage while covering nothing.\nlog:\n%v", *logged)
	}
}

// TestTheProbeRunsAsFixedArgv. AGENTS.md forbids `sh -c` outright, and this is the only field in the pool
// declaration that is a COMMAND. A probe that reached a shell would make an operator-declared string into
// remote code execution with metacharacters.
func TestTheProbeRunsAsFixedArgv(t *testing.T) {
	r := &appProbeRunner{guestRunning: true, probeExit: 0}
	e, _ := appProbeEngine(t, r, "curl -sf http://127.0.0.1:3000/api/user")

	if _, err := e.verifyRepaired(context.Background(), Outstanding{
		Host: "guest-a", Class: ClassDeviceDown, Node: "node-1", FaultRef: "101",
	}); err != nil {
		t.Fatalf("verifyRepaired: %v", err)
	}
	if len(r.probeArgv) != 3 || r.probeArgv[0] != "curl" {
		t.Fatalf("the probe was not split into fixed argv: %#v", r.probeArgv)
	}
	for _, a := range r.probeArgv {
		if a == "-c" || strings.HasSuffix(a, "sh") {
			t.Errorf("the probe argv routes through a shell: %#v", r.probeArgv)
		}
	}
}

// TestValidHealthProbeRefusesShellSyntax. Refusing at DECLARATION time is what keeps the declaration and
// the behaviour from diverging: an operator writing a pipeline would otherwise hand curl the literal
// arguments "|" and "grep" and believe they had declared a data-path check.
func TestValidHealthProbeRefusesShellSyntax(t *testing.T) {
	for _, bad := range []string{
		"curl -sf localhost/api | grep ok",
		"curl -sf localhost/api; true",
		"curl -sf localhost/api > /tmp/x",
		"echo $(whoami)",
		"a && b",
		"",
		"   ",
	} {
		if err := ValidHealthProbe(bad); err == nil {
			t.Errorf("ValidHealthProbe(%q) accepted a declaration that cannot run as written", bad)
		}
	}
	// Vacuity floor: a real probe must still be accepted, or the refusal above is just a broken validator
	// and no probe can ever be declared.
	for _, good := range []string{
		"curl -sf http://127.0.0.1:3000/api/user",
		"/usr/local/bin/tg-app-health",
		"psql -U postgres -c select1",
	} {
		if err := ValidHealthProbe(good); err != nil {
			t.Errorf("ValidHealthProbe(%q) refused a legitimate probe: %v", good, err)
		}
	}
}
