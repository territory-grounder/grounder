package main

import "testing"

// TG-363 follow-through — the guard must be able to say WHICH BUILD IT IS.
//
// The fix that gave "no such log" its own exit status sat merged-and-undeployed for a day: both syslog
// hosts kept serving the older build while main carried a consumer expecting the new status, and nothing
// detected it. Nothing could have. This is a stripped static binary, so a drift check has no string to
// compare — the sibling shell guards are checksummable against their repo copies, and the one artifact
// that actually drifted is precisely the one that is not.
//
// So the binary now answers `-version`. These oracles pin the part that carries risk: a version probe
// must never become a way to switch the guard off.

// THE LOAD-BEARING CASE. An operator can edit authorized_keys, and
//
//	command="/usr/local/sbin/tg-syslogng-guard -version"
//
// is a plausible thing to write while checking a rollout. If argv alone decided this, every read on that
// host would print a stamp and exit 0 — the guard disabled, silently, while reporting success. It must
// instead fall through to the normal path, where the request is validated (and an empty command refused).
func TestAVersionFlagCannotDisableTheGuardOverSSH(t *testing.T) {
	if versionRequested([]string{"tg-syslogng-guard", "-version"}, true) {
		t.Fatal("an SSH invocation carrying -version in the forced command was treated as a version probe: " +
			"the guard would print a stamp and exit 0 for EVERY read on that host, which is a fail-OPEN " +
			"disabling of the control disguised as success")
	}
	if versionRequested([]string{"tg-syslogng-guard", "--version"}, true) {
		t.Error("same defect via the long form")
	}
}

// The probe must still work locally, or the stamp is unreachable and the drift problem is unsolved.
func TestALocalVersionProbeIsHonoured(t *testing.T) {
	if !versionRequested([]string{"tg-syslogng-guard", "-version"}, false) {
		t.Error("-version off an SSH session must be honoured, or the binary still cannot say which build " +
			"it is and TG-363's undeployed-fix gap stays undetectable")
	}
	if !versionRequested([]string{"tg-syslogng-guard", "--version"}, false) {
		t.Error("--version must work too")
	}
}

// An ordinary invocation is never a version probe, in either mode — otherwise the guard would exit 0
// without reading anything.
func TestAnOrdinaryInvocationIsNotAVersionProbe(t *testing.T) {
	for _, sshSet := range []bool{true, false} {
		if versionRequested([]string{"tg-syslogng-guard"}, sshSet) {
			t.Errorf("a bare invocation (sshCommandSet=%v) must not be a version probe", sshSet)
		}
	}
	if versionRequested([]string{"tg-syslogng-guard", "tail"}, false) {
		t.Error("an unrelated first argument must not be a version probe")
	}
}

// The stamp must be a real value in a shipped build. `unstamped` is the source default and is what a
// build missing its -ldflags produces — which would put us straight back to an unidentifiable binary.
func TestTheStampDefaultIsHonestAboutBeingUnset(t *testing.T) {
	if buildStamp == "" {
		t.Fatal("an EMPTY stamp prints a blank line and reads like a successful probe of an unknown build; " +
			"the default must say what it is")
	}
	if buildStamp != "unstamped" {
		t.Logf("built with a stamp: %q", buildStamp)
	}
}
