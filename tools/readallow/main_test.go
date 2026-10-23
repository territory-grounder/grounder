package main

// ORACLES FOR THE READ-ONLY ALLOWLIST GENERATOR (TG-280).
//
// The generator's whole job is to make the host guard's allowlist a PROJECTION of the diagnostic
// catalogue rather than a hand-copied second list. tools/guardallow exists because the actuation
// allowlist was hand-authored and drifted from the op-class registry, and TG then chose an action that
// cleared all six of its own gates and died at the host with exit 42.
//
// The same drift here would be worse, not better. A denied READ returns the sentinel
// "(<host> was unreachable or the read errored)" — a perfectly ordinary-looking answer. So a drifted
// read allowlist does not fail loudly; it makes the agent silently blind while the estate looks quiet.
// That is exactly TG-271, which ran for weeks with nobody noticing.

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/modules/observability/hostdiag"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// KILLING MUTATION: return the list instead of an error when the catalogue is empty. RED — an empty
// allowlist installs cleanly and denies every read, and the operator sees a quiet estate, not a broken
// one. Emitting nothing must fail where somebody is looking.
func TestAnEmptyCatalogueIsAnErrorNotAnEmptyAllowlist(t *testing.T) {
	if _, err := render(nil, 0, nil, 500); err == nil {
		t.Fatal("rendered an allowlist from an empty catalogue with no error — installing it would deny " +
			"every host read, and this lane reports a denied read as an unreachable host, so the estate " +
			"would look quiet rather than blind")
	}
	// A non-zero check count with zero commands is the subtler version of the same bug.
	if _, err := render(nil, 4, nil, 500); err == nil {
		t.Fatal("four checks rendered zero commands and that was accepted — the projection is broken")
	}
}

// The allowlist must be byte-identical to what the CLIENT sends — the guard compares with `grep -qxF`,
// so one different byte denies the read.
//
// BE HONEST ABOUT WHAT THIS PROVES. It cannot detect the quoting itself changing, because both sides go
// through syslogng.RemoteCommand and would move together. That is not a hole, it is the design: ONE
// implementation makes the agreement true by construction, which is stronger than any test comparing two
// implementations could be. What this test does prove is the remaining risk — that render() TRANSFORMS
// or drops catalogue lines on the way through.
//
// KILLING MUTATION: have render() rewrite or filter the catalogue lines. RED.
func TestEveryLineIsExactlyWhatTheClientPutsOnTheWire(t *testing.T) {
	got, err := render(hostdiag.ReadOnlyWireCommands(), hostdiag.ReadOnlyCheckCount(), nil, 500)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := map[string]struct{}{}
	for _, c := range hostdiag.ReadOnlyWireCommands() {
		want[c] = struct{}{}
	}
	seen := 0
	for _, line := range got {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if _, ok := want[line]; !ok {
			t.Errorf("allowlist line %q is not something the client ever sends — it is dead weight at "+
				"best, and at worst it authorises a command TG never issues", line)
		}
		seen++
	}
	if seen != len(want) {
		t.Fatalf("emitted %d command(s) for a catalogue of %d — a command the client sends but the "+
			"allowlist omits is a read that silently returns 'host unreachable' forever", seen, len(want))
	}
}

// The catalogue is the security boundary, so its shape is asserted rather than assumed. If a MUTATING
// command ever appears in it, this list stops being a read-only grammar and the guard stops being a
// meaningful control.
//
// KILLING MUTATION: add {"systemctl","restart","nginx"} to the hostdiag catalogue. RED.
func TestTheReadOnlyGrammarContainsNothingThatMutates(t *testing.T) {
	forbidden := []string{"restart", "start", "stop", "reload", "rm", "kill", "chmod", "chown",
		"mount", "reboot", "shutdown", "dd", "tee", "install", "apt", "yum", "systemctl set-"}
	cmds := hostdiag.ReadOnlyWireCommands()
	if len(cmds) == 0 {
		t.Fatal("the catalogue is empty — this assertion would pass by examining nothing")
	}
	for _, c := range cmds {
		for _, f := range forbidden {
			if strings.Contains(c, "'"+f+"'") || strings.Contains(c, "'"+f) && strings.HasPrefix(c, "'"+f) {
				t.Errorf("read-only catalogue contains a mutating verb %q: %s\n"+
					"The guard's whole value is that a leaked diagnostic key cannot change anything. "+
					"One mutating entry here silently converts it into a write key.", f, c)
			}
		}
	}
}

// A log path containing whitespace or a quote would produce a line whose meaning depends on remote shell
// splitting — which is precisely what the single-quoting exists to prevent.
func TestALogPathThatCouldWordSplitIsRefused(t *testing.T) {
	for _, bad := range []string{"/var/log/my log", "/var/log/x'y", "/var/log/a\nb"} {
		if _, err := render([]string{"'uptime'"}, 1, []string{bad}, 500); err == nil {
			t.Errorf("accepted log path %q — the rendered line's meaning would depend on how the remote "+
				"shell splits it, and the guard's exact match would then be matching the wrong thing", bad)
		}
	}
}

// The tail shapes must be a DISCRETE enumerated set, not a range: the guard matches exact strings, and a
// bounded set also caps how much of a log a leaked key can pull in one call.
func TestTailSizesAreEnumeratedAndBounded(t *testing.T) {
	got, err := render([]string{"'uptime'"}, 1, []string{"/var/log/syslog"}, 200)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var tails int
	for _, l := range got {
		if strings.HasPrefix(l, "'tail'") {
			tails++
			if l == syslogng.RemoteCommand([]string{"tail", "-n", "500", "--", "/var/log/syslog"}) {
				t.Error("emitted a -n 500 tail under a max of 200 — the cap does not bind, so a leaked " +
					"key can pull more of the log than the operator allowed")
			}
		}
	}
	if tails == 0 {
		t.Fatal("no tail lines emitted for a configured log path — the syslog reader would be denied " +
			"every read while appearing to be configured")
	}
}
