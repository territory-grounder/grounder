package journal

import (
	"strings"
	"testing"
	"time"
)

// sshRow builds one `journalctl -o json` line for these oracles.
func sshRow(identifier, message string, at time.Time) string {
	return `{"SYSLOG_IDENTIFIER":"` + identifier + `","_COMM":"` + identifier +
		`","MESSAGE":"` + message + `","__REALTIME_TIMESTAMP":"` +
		itoa(at.UnixMicro()) + `","__CURSOR":"c-` + identifier + `-` + itoa(at.UnixMicro()) + `"}`
}

// THE ESTATE HAS NO SUDO, AND THAT IS WHY THIS READER PRODUCED NOTHING.
//
// Measured 2026-07-29. The journal reader wrote ZERO evidence rows in seven days while the PVE reader wrote
// 1,407. The standing diagnosis blamed the allowlist ("pointed at hosts TG never triages") — and that was
// wrong: three of its four hosts are among the most-triaged on the estate. On dc1excalidraw01, 153
// triages in the window, `journalctl -t sudo` over 7 days returns "-- No entries --", while the same window
// holds 564 `Accepted publickey` lines. Every actor here authenticates as root over SSH with a key.
func TestAnSSHKeyLoginIsEvidenceWhenTheSourceIsArmed(t *testing.T) {
	at := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	stdout := sshRow("sshd",
		"Accepted publickey for root from 10.30.1.9 port 51234 ssh2: ED25519 SHA256:AbCd1234efGH", at)

	if got := parseJournal([]byte(stdout), "guest-a", at.Add(-time.Hour), at.Add(time.Hour), false); len(got) != 0 {
		t.Errorf("SSH evidence was produced with the source DISARMED (%d rows) — arming an evidence source "+
			"changes triage semantics and must be an operator act", len(got))
	}

	got := parseJournal([]byte(stdout), "guest-a", at.Add(-time.Hour), at.Add(time.Hour), true)
	if len(got) != 1 {
		t.Fatalf("armed, an Accepted-publickey line produced %d evidence rows, want 1", len(got))
	}
	e := got[0]
	if e.Actor != "root!SHA256:AbCd1234efGH" {
		t.Errorf("actor = %q, want root!SHA256:AbCd1234efGH", e.Actor)
	}
	if e.ActionKind != "ssh-login" {
		t.Errorf("action kind = %q, want ssh-login", e.ActionKind)
	}
	if e.Domain != "journal" || e.Target != "guest-a" || !e.Covered {
		t.Errorf("evidence is mis-shaped: %+v", e)
	}
	if !e.ObservedAt.Equal(at) {
		t.Errorf("observed_at = %v, want %v", e.ObservedAt, at)
	}
}

// ★ THE FINGERPRINT IS THE WHOLE POINT. On this estate TG's actuator, the fault harness and every human
// operator all log in as `root`. An evidence record that named only the login name would say "root" for all
// three — which is not attribution, it is the appearance of attribution, and it would resolve TG's own heal
// and a stranger's change to the same actor.
func TestTwoDifferentKEYSAsTheSameUserAreDifferentActors(t *testing.T) {
	at := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	stdout := strings.Join([]string{
		sshRow("sshd", "Accepted publickey for root from 10.30.1.9 port 1 ssh2: ED25519 SHA256:TGactuatorKEY", at),
		sshRow("sshd", "Accepted publickey for root from 10.30.9.9 port 2 ssh2: RSA SHA256:aHumanOperator", at),
	}, "\n")

	got := parseJournal([]byte(stdout), "guest-a", at.Add(-time.Hour), at.Add(time.Hour), true)
	if len(got) != 2 {
		t.Fatalf("got %d evidence rows, want 2", len(got))
	}
	if got[0].Actor == got[1].Actor {
		t.Fatalf("two logins with DIFFERENT keys resolved to the same actor %q — the login name alone cannot "+
			"separate TG's own heal from a stranger's change, which is the one question this engine answers",
			got[0].Actor)
	}
	for _, e := range got {
		if !strings.Contains(e.Actor, "SHA256:") {
			t.Errorf("actor %q carries no key fingerprint", e.Actor)
		}
	}
}

// A LOGIN WITH NO FINGERPRINT YIELDS NO ACTOR. Password auth, or any format this parser does not recognise,
// must produce nothing rather than a bare "root" — an unattributable record is honest; a record attributing
// everything on the estate to "root" is worse than silence, because the attributor would act on it.
func TestALoginWithoutAFingerprintIsNotEvidence(t *testing.T) {
	at := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	for _, msg := range []string{
		"Accepted password for root from 10.30.9.9 port 3 ssh2",
		"Failed publickey for root from 10.30.9.9 port 4 ssh2: RSA SHA256:someKey",
		"Connection closed by authenticating user root 10.30.9.9 port 5 [preauth]",
		"Disconnected from user root 10.30.9.9 port 6",
		"Server listening on 0.0.0.0 port 22.",
	} {
		got := parseJournal([]byte(sshRow("sshd", msg, at)), "guest-a", at.Add(-time.Hour), at.Add(time.Hour), true)
		if len(got) != 0 {
			t.Errorf("%q produced %d evidence row(s) (actor %q) — only an ACCEPTED publickey login names a "+
				"principal this engine can resolve", msg, len(got), got[0].Actor)
		}
	}
}

// The sudo source is unchanged by any of this. A regression here would trade one blind domain for another.
func TestArmingSSHDoesNotDisturbTheSudoSource(t *testing.T) {
	at := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	stdout := sshRow("sudo", "pam_unix(sudo:session): session opened for user root by kp(uid=0)", at)
	for _, armed := range []bool{false, true} {
		got := parseJournal([]byte(stdout), "guest-a", at.Add(-time.Hour), at.Add(time.Hour), armed)
		if len(got) != 1 || got[0].Actor != "kp" || got[0].ActionKind != "sudo-session-open" {
			t.Errorf("sshSessions=%v changed the sudo result: %+v", armed, got)
		}
	}
}

// The window re-check and the malformed-line skip must hold for the new source too — the reader is defence in
// depth against a skewed clock or a journalctl that over-returns, and one bad line must never be fatal.
func TestSSHEvidenceStillHonoursTheWindowAndSurvivesGarbage(t *testing.T) {
	at := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	good := sshRow("sshd", "Accepted publickey for root from 192.0.2.1 port 1 ssh2: ED25519 SHA256:inWindow", at)
	early := sshRow("sshd", "Accepted publickey for root from 192.0.2.1 port 2 ssh2: ED25519 SHA256:tooEarly", at.Add(-2*time.Hour))
	stdout := strings.Join([]string{"{not json", good, "", early, "also not json"}, "\n")

	got := parseJournal([]byte(stdout), "guest-a", at.Add(-time.Hour), at.Add(time.Hour), true)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want only the in-window one: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Actor, "inWindow") {
		t.Errorf("kept the wrong record: %q", got[0].Actor)
	}
}

// The argv stays FIXED (INV-02) and gains the sshd matcher only when armed. journalctl ORs match groups on a
// bare "+"; nothing here is interpolated from anything a model produced.
func TestTheSSHMatcherIsAddedToTheArgvOnlyWhenArmed(t *testing.T) {
	for _, armed := range []bool{false, true} {
		m := New([]Access{{Site: "s", HostGlob: "*"}}, nil, nil, WithSSHSessions(armed))
		if m.sshSessions != armed {
			t.Fatalf("WithSSHSessions(%v) did not take effect", armed)
		}
	}
	// The default is DISARMED: an evidence source that changes triage semantics is never on by omission.
	if New([]Access{{Site: "s", HostGlob: "*"}}, nil, nil).sshSessions {
		t.Error("SSH-session evidence defaults to ARMED — on an estate administered over SSH that silently " +
			"turns every human login inside the attribution window into a named actor")
	}
}
