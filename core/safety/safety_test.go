package safety

import (
	"errors"
	"testing"
)

// Band zero value must be the most-restrictive band, so any un-set/errored classification fails closed.
func TestBandZeroValueFailsClosed(t *testing.T) {
	var b Band // zero value
	if b != BandPollPause {
		t.Fatalf("zero Band = %v, want BandPollPause", b)
	}
	if b.String() != "POLL_PAUSE" {
		t.Fatalf("zero Band.String() = %q, want POLL_PAUSE", b.String())
	}
	// An out-of-range value must also render as POLL_PAUSE (fail closed), never as AUTO.
	if Band(99).String() != "POLL_PAUSE" {
		t.Fatalf("invalid Band did not fail closed: %q", Band(99).String())
	}
}

func TestNeverAutoFloor(t *testing.T) {
	for _, op := range []string{"mkfs", "dropdb", "zpool-destroy", "reboot", "credential-revoke"} {
		if !IsNeverAuto(op) {
			t.Errorf("%q must be on the never-auto floor", op)
		}
	}
	if IsNeverAuto("restart-service") {
		t.Errorf("reversible op must not be on the never-auto floor")
	}
	// A case or whitespace variant must NOT slip past the floor (fail closed) — regression for the
	// adapter floor-clamp case-sensitivity bypass.
	for _, v := range []string{"Reboot", "REBOOT", " reboot ", "Kubectl-Delete", "  kubectl-drain"} {
		if !IsNeverAuto(v) {
			t.Errorf("floor variant %q must be clamped (case/whitespace-insensitive)", v)
		}
	}
}

func TestFailLaneZeroValueIsRemediation(t *testing.T) {
	var l FailLane // zero value must be the fail-CLOSED lane
	if l != LaneRemediation {
		t.Fatalf("zero FailLane must be LaneRemediation (fail closed)")
	}
}

// The mode chokepoint absorbs the retired MutationGate (REQ-1520): actuation is off by default (no bound
// mode ⇒ read-only), and a fresh actuating chokepoint (mode Semi/Full + preflight green) actuates.
func TestChokepointMayActuateAbsorbsGate(t *testing.T) {
	// A read-only chokepoint (no mode authority) is the grounder / un-bound worker posture: never actuates.
	ro := NewReadOnlyChokepoint()
	if ro.MayActuate() {
		t.Fatal("a read-only chokepoint must never actuate (fail-closed zero value)")
	}
	if err := ro.GuardMutation(); err != ErrMutationDisabled {
		t.Fatalf("GuardMutation must deny while read-only, got %v", err)
	}
	// The pure authority: mode ∈ {Semi,Full} AND preflight green. Any missing input ⇒ false.
	on := NewFixedModeAuthority(true).CurrentActuationMode()
	off := NewFixedModeAuthority(false).CurrentActuationMode()
	if MayActuate(on, false) || MayActuate(off, true) || MayActuate(nil, true) {
		t.Fatal("MayActuate must be false unless mode actuates AND preflight is green (and mode non-nil)")
	}
	if !MayActuate(on, true) {
		t.Fatal("MayActuate must be true when the mode actuates and preflight is green")
	}
	// A fully actuating chokepoint permits actuation; GuardMutation passes.
	act := NewActuatingChokepoint()
	if !act.MayActuate() || act.GuardMutation() != nil {
		t.Fatal("an actuating chokepoint (mode Semi/Full + preflight green) must permit actuation")
	}
}

// fakeProver stands in for a wired (or unwired) interception chain in safety-core unit tests.
type fakeProver struct{ err error }

func (f fakeProver) SelfTest() error { return f.err }

// ProvePreflight discharges the proof obligation (a passing prover) that marks the preflight green — the
// successor to the proof half of the retired EnableMutation (REQ-1520/1521). A nil chokepoint, a nil prover, or
// a failing prover all fail closed, leaving the preflight ungreen. Crucially, marking the preflight green does
// NOT actuate: the mode still governs, so a preflight-green chokepoint with no actuating mode stays read-only.
func TestProvePreflightMarksGreenWithoutActuating(t *testing.T) {
	if err := (*Chokepoint)(nil).ProvePreflight(fakeProver{}); err == nil {
		t.Fatal("ProvePreflight must refuse a nil chokepoint")
	}
	// nil prover ⇒ refuse, preflight stays red.
	c := NewChokepoint(NewFixedModeAuthority(true))
	if err := c.ProvePreflight(nil); err == nil || c.IsPreflightGreen() {
		t.Fatalf("ProvePreflight must refuse a nil prover and touch nothing: err=%v green=%v", err, c.IsPreflightGreen())
	}
	// failing prover ⇒ refuse, preflight NOT marked green (proof precedes the mark).
	c2 := NewChokepoint(NewFixedModeAuthority(true))
	if err := c2.ProvePreflight(fakeProver{err: errors.New("unwired")}); err == nil || c2.IsPreflightGreen() || c2.MayActuate() {
		t.Fatalf("ProvePreflight must refuse a failing prover and leave preflight red: err=%v green=%v", err, c2.IsPreflightGreen())
	}
	// passing prover ⇒ preflight green, and — because the bound mode actuates — MayActuate becomes true. With a
	// non-actuating mode a green preflight alone must NOT actuate (the mode is the source of truth).
	cShadow := NewChokepoint(NewFixedModeAuthority(false))
	if err := cShadow.ProvePreflight(fakeProver{}); err != nil || !cShadow.IsPreflightGreen() {
		t.Fatalf("ProvePreflight must mark green behind a passing prover: err=%v green=%v", err, cShadow.IsPreflightGreen())
	}
	if cShadow.MayActuate() {
		t.Fatal("a green preflight with a non-actuating mode must NOT actuate — the mode governs")
	}
}

func TestValidVerdict(t *testing.T) {
	if !ValidVerdict(VerdictMatch) || !ValidVerdict(VerdictPartial) || !ValidVerdict(VerdictDeviation) {
		t.Fatal("the three mechanical verdicts must be valid")
	}
	if ValidVerdict(Verdict("")) || ValidVerdict(Verdict("success")) {
		t.Fatal("unknown verdict must be invalid (callers treat as deviation)")
	}
}

func TestHighRiskCategory(t *testing.T) {
	for _, c := range []string{"maintenance", "security-incident", "deployment", "  Maintenance ", "DEPLOYMENT"} {
		if !HighRiskCategory(c) {
			t.Errorf("category %q must be high-risk (case/space-insensitive)", c)
		}
	}
	for _, c := range []string{"", "availability", "resource", "generic", "unknown"} {
		if HighRiskCategory(c) {
			t.Errorf("category %q must NOT be high-risk (mechanical floor still governs it)", c)
		}
	}
}

func TestIsRestartClass(t *testing.T) {
	for _, s := range []string{"systemctl restart nginx", "docker compose restart", "kubectl rollout restart deploy/x", "pct reboot 101", "restart-service"} {
		if !IsRestartClass(s) {
			t.Errorf("%q must be restart-class", s)
		}
	}
	for _, s := range []string{"systemctl status nginx", "kubectl get pods", "dropdb prod", ""} {
		if IsRestartClass(s) {
			t.Errorf("%q must NOT be restart-class", s)
		}
	}
}

// A2 (integration-audit): the self-protected control-plane guard is fed only the terse (op, op_class) pair —
// NOT the built argv — so IsRestartClass MUST recognize the bare op_class token of EVERY actuatable restart/
// reload class, or SelfProtectedRestart silently never fires for that class. Guard all three curated classes
// exactly as temporal/runner/activities.go calls it: IsRestartClass(op, op_class).
func TestIsRestartClassCoversEveryActuatableClass(t *testing.T) {
	cases := []struct{ op, opClass string }{
		{"restart", "restart-service"},
		{"reload", "reload-service"},
		{"start", "start-service"},
		{"restart", "restart-container"},
	}
	for _, c := range cases {
		if !IsRestartClass(c.op, c.opClass) {
			t.Errorf("SelfProtectedRestart is DEAD for op=%q op_class=%q — IsRestartClass must recognize it (self-protected control-plane veto would never fire)", c.op, c.opClass)
		}
	}
}

// The floor is immutable-by-construction: it is unexported, so no package can delete or add an entry at
// runtime. NeverAutoClasses returns a COPY — mutating the returned slice cannot lift a floor op (the
// Phase-2 readiness review's §4.B.8 hardening; an exported mutable map would be a live-canary attack
// surface). This test asserts the accessor is copy-safe and the canonical destructive classes are present.
func TestNeverAutoFloorIsImmutableCopy(t *testing.T) {
	got := NeverAutoClasses()
	n := len(got)
	// Corrupting the returned slice must not change the floor.
	for i := range got {
		got[i] = "lifted"
	}
	if len(NeverAutoClasses()) != n {
		t.Fatal("NeverAutoClasses must return a fresh copy each call")
	}
	for _, must := range []string{"reboot", "dropdb", "kubectl-delete", "mkfs", "shutdown", "credential-revoke"} {
		if !IsNeverAuto(must) {
			t.Errorf("floor must still contain %q after a caller corrupts a copy", must)
		}
	}
	// Case/whitespace variants still hit the floor (fail-closed normalization).
	if !IsNeverAuto("  Reboot ") || !IsNeverAuto("KUBECTL-DELETE") {
		t.Fatal("normalization must keep case/whitespace variants on the floor")
	}
}
