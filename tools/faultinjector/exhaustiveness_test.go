package faultinjector

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
)

// EVERY REGISTRATION POINT, KEYED ON THE CLOSED ENUMERATION.
//
// Adding a fault class means touching a switch in five different files. container-down shipped fully
// implemented and still unschedulable because ONE accept-list had never heard of it; the CLI got a guard for
// that (cmd/faultinjector/classes_test.go), and the other registration points did not. Behaviour tests cannot
// close this gap — they name the class they exercise, so a class nobody wrote a test for is a class nothing
// checks. The only oracle that survives the NEXT class is one driven by AllClasses().
//
// Each test below drives a FULLY-POPULATED decision, so a failure means the wiring is missing, never that the
// fixture was thin.

func fullGuest() PoolGuest {
	return PoolGuest{VMID: "999", Name: "guest-under-test", Node: "node-under-test",
		Container: "declared-container", Unit: "declared.service", LogPath: "/var/log/tg-demo/app.log"}
}

// TestEveryClassHasAFaultRefRule — the handle an undo needs. Before this was extracted, the fallback was the
// guest VMID, which is a plausible-looking value for EVERY class: an unwired class would record a device-down
// handle, and its undo would then start a guest that was never stopped while the real fault stayed put.
func TestEveryClassHasAFaultRefRule(t *testing.T) {
	if len(AllClasses()) == 0 {
		t.Fatal("AllClasses is empty — this oracle would pass vacuously")
	}
	for _, c := range AllClasses() {
		ref, known := faultRefFor(Decision{Act: true, Guest: fullGuest(), Class: c})
		if !known {
			t.Errorf("class %q has no fault_ref rule — its obligation would be recorded with a guessed handle "+
				"that cannot undo it", c)
			continue
		}
		if c.OwesRestore() && strings.TrimSpace(ref) == "" {
			t.Errorf("class %q owes a restore but its fault_ref is empty — the ledger cannot know what to "+
				"undo, and the fault strands", c)
		}
	}
}

// TestEveryRestoringClassHasAnUndo — UndoArgv is what the reconciler actually runs. A class that owes a
// restore and has no undo is a fault with no way back.
func TestEveryRestoringClassHasAnUndo(t *testing.T) {
	for _, c := range AllClasses() {
		if !c.OwesRestore() {
			continue
		}
		ref, _ := faultRefFor(Decision{Act: true, Guest: fullGuest(), Class: c})
		argv, host, err := UndoArgv(Outstanding{ID: 1, Host: "guest-under-test", Class: c,
			Node: "node-under-test", FaultRef: ref})
		if err != nil {
			t.Errorf("class %q owes a restore but UndoArgv refuses it: %v", c, err)
			continue
		}
		if len(argv) == 0 || strings.TrimSpace(host) == "" {
			t.Errorf("class %q undo is incomplete: argv=%v host=%q", c, argv, host)
		}
	}
}

// TestEveryRestoringClassArmsOnAHostThatSurvivesTheFault is the placement oracle, and it is the one with a
// live incident behind it: docuseal01's cleanup was armed INSIDE the guest that was then stopped, so the
// timer died with its target and the fill was never cleaned. Only device-down removes its own host, so only
// device-down may arm on the node; every other class must arm on the guest.
func TestEveryRestoringClassArmsOnAHostThatSurvivesTheFault(t *testing.T) {
	g := fullGuest()
	for _, c := range AllClasses() {
		if !c.OwesRestore() {
			continue
		}
		host, undo, armed := armFor(Decision{Act: true, Guest: g, Class: c})
		if !armed {
			t.Errorf("class %q owes a restore but has no arm rule — it would silently inherit another class's "+
				"undo, which reads in the log as a successful arm", c)
			continue
		}
		if len(undo) == 0 {
			t.Errorf("class %q armed with an empty undo", c)
		}
		want := g.Name // the guest survives every class except device-down
		if c == ClassDeviceDown {
			want = g.Node
		}
		if host != want {
			t.Errorf("class %q arms its restore on %q, want %q — a timer armed on a host the fault removes "+
				"dies with it (the docuseal01 stranding)", c, host, want)
		}
	}
}

// TestEveryClassHasAVerifierOrIsRefused — the fail-open this file was written after. verifyRepaired's default
// returned (true, nil): "verified repaired" without looking. The caller answers true with MarkRestored, which
// is PERMANENT, so an unwired class closed its obligation unverified and stranded its fault forever.
func TestEveryClassHasAVerifierOrIsRefused(t *testing.T) {
	e := &Engine{Exec: verifyProbeRunner{}, Log: func(string, ...any) {}}
	for _, c := range AllClasses() {
		if !c.OwesRestore() {
			continue
		}
		ok, err := e.verifyRepaired(context.Background(), Outstanding{ID: 1, Host: "h", Class: c,
			Node: "n", FaultRef: "ref", RestoreDueAt: time.Now()})
		if err != nil && strings.Contains(err.Error(), "no verifier wired") {
			t.Errorf("class %q owes a restore and has NO verifier — its obligation would be closed on an "+
				"assumption", c)
			continue
		}
		_ = ok // the probe runner's answer is irrelevant; that a verifier RAN is the property
	}
}

// TestAnUnknownClassIsRefusedEverywhere is the negative half. Without it, every test above could pass because
// the fallbacks are permissive rather than because the wiring exists.
func TestAnUnknownClassIsRefusedEverywhere(t *testing.T) {
	const bogus Class = "not-a-real-class"
	for _, c := range AllClasses() {
		if c == bogus {
			t.Fatalf("%q is a declared class — pick a different sentinel", bogus)
		}
	}
	d := Decision{Act: true, Guest: fullGuest(), Class: bogus}

	if _, known := faultRefFor(d); known {
		t.Error("an unknown class was given a fault_ref — a guessed handle cannot undo anything")
	}
	if _, _, armed := armFor(d); armed {
		t.Error("an unknown class was given an arm rule — it would inherit another class's undo")
	}
	if _, _, err := UndoArgv(Outstanding{ID: 1, Class: bogus, FaultRef: "x"}); err == nil {
		t.Error("an unknown class was given an undo")
	}
	e := &Engine{Exec: verifyProbeRunner{}, Log: func(string, ...any) {}}
	ok, err := e.verifyRepaired(context.Background(), Outstanding{ID: 1, Class: bogus, FaultRef: "x"})
	if ok || err == nil {
		t.Errorf("an unknown class verified as REPAIRED (ok=%v err=%v) — MarkRestored is permanent, so this "+
			"strands the fault forever", ok, err)
	}
}

// TestEveryProvokedOpClassIsRegistered closes the other direction of the opcover pairing at the source. A
// Provokes() entry naming a slug that is not in the live op-class registry silently credits coverage that
// does not exist: opcover would report the fault class as covering something no interceptor can actuate.
func TestEveryProvokedOpClassIsRegistered(t *testing.T) {
	for _, c := range AllClasses() {
		for _, op := range c.Provokes() {
			if _, ok := opschema.Lookup(op); !ok {
				t.Errorf("fault class %q declares it provokes %q, which is NOT a registered op-class — that "+
					"credits coverage the estate cannot act on", c, op)
			}
		}
	}
}

// verifyProbeRunner answers every command plausibly. The verifier oracles care that a verifier RAN, not what
// it concluded, so this must never be the thing that decides a test.
type verifyProbeRunner struct{}

func (verifyProbeRunner) Run(_ context.Context, _ string, argv []string) (string, int, error) {
	if len(argv) > 0 && argv[0] == "test" {
		return "", 1, nil // VerifyFillRemoved: `test -e` non-zero == the file is gone
	}
	return "running\nactive\n", 0, nil
}

// TestADetectionOnlyClassClaimsNoRemediationCoverage — a fault class must not claim to provoke an op-class it
// cannot legitimately drive. That is a FALSE coverage declaration, and it is worse than a declared gap: it
// silently credits coverage, which is exactly what opcover exists to catch, made by opcover's own input.
//
// disk-fill is the case that forced this. It fallocates a SYNTHETIC benchmark artifact
// (/var/tmp/tg-tier1-fill.img); `disk-grow` does not remediate a rogue file, and the only real remediation —
// removing the injector's own file — is something TG must never learn. Measured: 74 faults, 12 proposals,
// 1 heal. mem-pressure has always been declared detection-only for the same reason.
func TestADetectionOnlyClassClaimsNoRemediationCoverage(t *testing.T) {
	detectionOnly := map[Class]string{
		ClassMemPressure: "a pressure signal, not a remediation driver",
		ClassDiskFill:    "fallocates a synthetic benchmark artifact no legitimate verb removes",
	}
	for c, why := range detectionOnly {
		if got := c.Provokes(); len(got) != 0 {
			t.Errorf("%s is detection-only (%s) but claims to provoke %v — that credits coverage which can "+
				"never legitimately be exercised", c, why, got)
		}
	}
	// and the guard on the guard: a class that DOES drive remediation must still declare it, or this test
	// would pass by making everything detection-only.
	driving := 0
	for _, c := range AllClasses() {
		if _, off := detectionOnly[c]; !off && len(c.Provokes()) > 0 {
			driving++
		}
	}
	if driving == 0 {
		t.Fatal("NO fault class provokes any op-class — the rotation drives no remediation at all, and every " +
			"coverage assertion in this suite would pass vacuously")
	}
}
