package hostdiag

// ORACLES FOR THE PROBE THAT ACTUALLY READS (TG-300 / TG-301).
//
// THE DEFECT, measured live on 2026-08-04. Three surfaces reported health over a lane that could not read
// a single host, and none of them was wrong on its own terms:
//
//	the configured key authenticated to 0 of 20 estate hosts   (every read failed)
//	module probe sweep: 10 ran, 10 ok, 0 failed                (the probe checked config, by design)
//	hostdiag.read: unobserved                                  (the register is fed only during triage)
//
// The probe's no-dial choice was REASONED — this connector's targets are whatever host alerts next, so
// certifying one host certifies the wrong thing. What that reasoning did not price is that the seam then
// has no observer outside triage, so "cannot read anything" and "nothing asked yet" render identically.
//
// So the probe reads: not to certify a host, but to prove the LANE, and to feed the register every sweep.

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// A REAL parseable key, because the config half of the probe runs first and a bad key would fail the test
// before it ever reaches the read this file is about.
func readProbeAccess(t *testing.T) []Access {
	t.Helper()
	return ParseAccess("dc1|dc1*|root|file:" + writeTestKey(t))
}

// KILLING MUTATION: restore the config-only probe (drop the probeRead call). RED — this is the exact
// production state: keys parse, known_hosts loads, every read fails, and the probe says ok.
func TestAProbeOverALaneThatCannotReadIsRed(t *testing.T) {
	attempted, produced := 0, 0
	m := NewModule(readProbeAccess(t), writeKnownHosts(t, knownHostLine(t, "dc1pve01"), knownHostLine(t, "dc1gpu01")),
		WithProbeRead(failRunner{}, accessResolver{accs: readProbeAccess(t)},
			func(p bool) {
				attempted++
				if p {
					produced++
				}
			}))
	res, err := m.SelfTest(context.Background(), "")
	if err == nil {
		t.Fatalf("probe returned GREEN (%q) over a lane where every read fails — that is the state that "+
			"ran for weeks: cheerful boot log, ok probe, blind agent", res.Summary)
	}
	if attempted != 1 || produced != 0 {
		t.Fatalf("register saw attempted=%d produced=%d, want 1/0 — a failing probe that reports nothing "+
			"leaves the seam UNOBSERVED, which is how this went unnoticed", attempted, produced)
	}
}

// The control: a lane that CAN read must go green, and must report produced — otherwise the alarm fires
// forever and gets muted, which is worse than no alarm.
func TestAWorkingLaneGoesGreenAndReportsProduced(t *testing.T) {
	attempted, produced := 0, 0
	m := NewModule(readProbeAccess(t), writeKnownHosts(t, knownHostLine(t, "dc1pve01"), knownHostLine(t, "dc1gpu01")),
		WithProbeRead(okRunner{}, accessResolver{accs: readProbeAccess(t)},
			func(p bool) {
				attempted++
				if p {
					produced++
				}
			}))
	res, err := m.SelfTest(context.Background(), "")
	if err != nil {
		t.Fatalf("a readable lane failed its probe: %v", err)
	}
	if attempted != 1 || produced != 1 {
		t.Fatalf("register saw attempted=%d produced=%d, want 1/1", attempted, produced)
	}
	// The summary must say a read HAPPENED. "authenticate-ready" alone is what made the old green
	// misleading, so a green that does not mention the read is a regression.
	if !contains(res.Summary, "READ ") {
		t.Fatalf("green summary %q does not state that a host was read — an operator reads this line and "+
			"concludes the lane works", res.Summary)
	}
}

// One unreachable host must NOT make the probe red. A pinned target turns an unrelated reboot into a red
// probe, and an operator who learns to ignore a red probe has lost the control.
func TestOneDeadHostDoesNotFailTheProbeWhenAnotherAnswers(t *testing.T) {
	m := NewModule(readProbeAccess(t), writeKnownHosts(t, knownHostLine(t, "dc1pve01"), knownHostLine(t, "dc1gpu01")),
		WithProbeRead(&firstFailsRunner{}, accessResolver{accs: readProbeAccess(t)}, nil))
	if _, err := m.SelfTest(context.Background(), ""); err != nil {
		t.Fatalf("probe went red though a candidate answered: %v — this is how an alarm becomes wallpaper", err)
	}
}

// KILLING MUTATION: make the no-runner path fall through to the read. RED at compile/behaviour — callers
// with no runner must keep the config-only probe, and its summary must NOT imply a read happened.
func TestAProbeWithNoRunnerSaysSoRatherThanImplyingARead(t *testing.T) {
	m := NewModule(readProbeAccess(t), writeKnownHosts(t, knownHostLine(t, "dc1pve01"), knownHostLine(t, "dc1gpu01")))
	res, err := m.SelfTest(context.Background(), "")
	if err != nil {
		t.Fatalf("config-only probe should still pass: %v", err)
	}
	if !contains(res.Summary, "NO read attempted") {
		t.Fatalf("summary %q does not disclose that nothing was read — a green that implies a read it did "+
			"not perform is the defect this ticket exists for", res.Summary)
	}
}

// VACUITY FLOOR: if no known_hosts name matches an allowlist glob there is no candidate, and that must be
// an ERROR. Silently skipping the read would restore the old always-green probe by another route.
func TestNoCandidateIsAnErrorNotASkippedRead(t *testing.T) {
	m := NewModule(readProbeAccess(t), writeKnownHosts(t, knownHostLine(t, "dc2other01")),
		WithProbeRead(okRunner{}, accessResolver{accs: readProbeAccess(t)}, nil))
	if _, err := m.SelfTest(context.Background(), ""); err == nil {
		t.Fatal("probe passed with NO candidate host — it performed no read and reported success, which " +
			"is the always-green behaviour this change removes")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// firstFailsRunner fails the first host and serves the second — an ordinary estate with one box down.
type firstFailsRunner struct{ n int }

func (r *firstFailsRunner) Run(_ context.Context, _ syslogng.Server, _ []string) (syslogng.RunResult, error) {
	r.n++
	if r.n == 1 {
		return syslogng.RunResult{}, errors.New("ssh: connect: no route to host")
	}
	return syslogng.RunResult{Stdout: []byte(" 12:00:00 up 5 days")}, nil
}
