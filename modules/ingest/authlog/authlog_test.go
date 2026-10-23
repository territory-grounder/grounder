package authlog

// Guards for TG's second witness (TG-315).
//
// Every fixture below that claims to be a real line IS one, copied from
// /mnt/logs/syslog-ng/dc1pve01/2026/08/dc1pve01-2026-08-05.log on 2026-08-06. That matters: the
// design this module started with would have admitted thousands of cron sessions per day, and only reading
// the estate's actual logs showed it.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const testYear = 2026

func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC) }
}

// THE DEFECT THE REAL LOGS REVEALED. 2,943 "session opened" lines in 36h, almost all CRON. Admitting them
// would produce thousands of events a day describing the crontab running — drowning the correlator, the
// precedent corpus, and the human approval queue, which is a worse outcome than the blindness this module
// replaces.
func TestCronRootSessionsAreNotAdmitted(t *testing.T) {
	real := "Aug  5 23:35:01 dc1pve01 CRON[2359236]: pam_unix(cron:session): session opened for user root(uid=0) by root(uid=0)"
	if e, ok := ParseLine("dc1pve01", real, testYear); ok {
		t.Errorf("a CRON root session was admitted as %q. There are ~2,000 of these per day on this "+
			"estate; one event each would flood the correlator and the approval queue with a description "+
			"of the crontab running.", e.Kind)
	}
	for _, svc := range []string{"systemd-user", "atd", "login"} {
		l := "Aug  5 23:35:01 h X[1]: pam_unix(" + svc + ":session): session opened for user root(uid=0) by root(uid=0)"
		if _, ok := ParseLine("h", l, testYear); ok {
			t.Errorf("a %s root session was admitted — only an sshd session is a login", svc)
		}
	}
	// The discriminator must still ADMIT the real case, or this test is only proving the module is deaf.
	ssh := "Aug  5 23:35:01 dc1pve01 sshd[123]: pam_unix(sshd:session): session opened for user root(uid=0) by (uid=0)"
	e, ok := ParseLine("dc1pve01", ssh, testYear)
	if !ok || e.Kind != KindRootSession {
		t.Fatalf("an sshd root session was NOT admitted (ok=%v kind=%q) — the discriminator rejects "+
			"everything and this module would be silent", ok, e.Kind)
	}
}

// Routine SUCCESS is not an event. 987 accepted publickey logins in 36h is the estate working.
func TestSuccessfulLoginsAreNotAdmitted(t *testing.T) {
	real := "Aug  5 23:32:00 dc1pve01 sshd-session[2336787]: Accepted publickey for root from 192.168.181.111 port 52202 ssh2: ED25519 SHA256:xZl5Oud53PTWH2d1Y3e51VFk4JhNAy1S4NSjd3KSPWg"
	if e, ok := ParseLine("dc1pve01", real, testYear); ok {
		t.Errorf("a routine successful login was admitted as %q — ~1,000/36h, and none of them is an "+
			"incident", e.Kind)
	}
}

func TestTheAdmittedSetIsAdmitted(t *testing.T) {
	cases := []struct {
		name, line string
		want       Kind
		principal  string
		ip         string
	}{
		{"failed password", `Aug  5 10:00:00 h sshd[1]: Failed password for root from 192.0.2.9 port 5 ssh2`, KindFailure, "root", "192.0.2.9"},
		{"invalid user", `Aug  5 10:00:00 h sshd[1]: Failed password for invalid user admin from 192.0.2.9 port 5 ssh2`, KindFailure, "admin", "192.0.2.9"},
		{"invalid user line", `Aug  5 10:00:00 h sshd[1]: Invalid user oracle from 192.0.2.9`, KindFailure, "oracle", "192.0.2.9"},
		{"pam auth failure", `Aug  5 10:00:00 h sshd[1]: pam_unix(sshd:auth): authentication failure; logname= uid=0 euid=0 tty=ssh ruser= rhost=192.0.2.9  user=root`, KindFailure, "root", "192.0.2.9"},
		{"sudo", `Aug  5 10:00:00 h sudo[1]:   kyriakos : TTY=pts/0 ; PWD=/ ; USER=root ; COMMAND=/bin/ls`, KindEscalation, "kyriakos", ""},
		{"su", `Aug  5 10:00:00 h su[1]: (to root) kyriakos on pts/0`, KindEscalation, "kyriakos", ""},
	}
	for _, c := range cases {
		e, ok := ParseLine("h", c.line, testYear)
		if !ok {
			t.Errorf("%s: NOT admitted — this is the evidence the cross-source correlation rule has no "+
				"second side without", c.name)
			continue
		}
		if e.Kind != c.want {
			t.Errorf("%s: kind = %q, want %q", c.name, e.Kind, c.want)
		}
		if e.Principal != c.principal {
			t.Errorf("%s: principal = %q, want %q", c.name, e.Principal, c.principal)
		}
		if e.SourceIP != c.ip {
			t.Errorf("%s: ip = %q, want %q", c.name, e.SourceIP, c.ip)
		}
	}
}

// A sudo command line and its pam session bookkeeping are ONE escalation. Counting both doubles every
// sudo, and a count that is wrong by 2x makes the burst threshold meaningless.
func TestSudoIsNotDoubleCounted(t *testing.T) {
	lines := []string{
		`Aug  5 10:00:00 h sudo[1]:   kyriakos : TTY=pts/0 ; PWD=/ ; USER=root ; COMMAND=/bin/ls`,
		`Aug  5 10:00:00 h sudo[1]: pam_unix(sudo:session): session opened for user root(uid=0) by kyriakos(uid=1000)`,
	}
	got := ParseLines("h", lines, testYear)
	if len(got) != 1 {
		t.Fatalf("one sudo produced %d event(s): %+v", len(got), got)
	}
	if got[0].Count != 1 {
		t.Errorf("one sudo counted %d times — the command line and its pam session are the same "+
			"escalation, and double-counting makes the burst threshold read 2x high", got[0].Count)
	}
}

// FOLDING IS THE FLOOD CONTROL. A brute-force burst is one fact; 247 envelopes would flood the correlator
// and the approval queue while telling an operator nothing the first ten lines did not.
func TestABurstFoldsToOneEventWithACount(t *testing.T) {
	var lines []string
	for i := 0; i < 247; i++ {
		lines = append(lines, `Aug  5 10:00:00 h sshd[1]: Failed password for root from 192.0.2.9 port 5 ssh2`)
	}
	got := ParseLines("h", lines, testYear)
	if len(got) != 1 {
		t.Fatalf("247 identical failures produced %d events, want 1 — unfolded, one brute-force run "+
			"admits 247 incidents and drowns every queue downstream", len(got))
	}
	if got[0].Count != 247 {
		t.Errorf("folded count = %d, want 247 — the count IS the signal; folding that loses it turns a "+
			"burst into a single failure", got[0].Count)
	}
	if sev := severityFor(got[0]); sev != "critical" {
		t.Errorf("a 247-failure burst graded %q, want critical", sev)
	}
}

// A single failure is not an incident. People mistype passwords, and a source that pages on that is a
// source an operator mutes.
func TestOneFailureIsNotGradedAsAnIncident(t *testing.T) {
	one := Event{Host: "h", Kind: KindFailure, Principal: "root", Count: 1}
	if sev := severityFor(one); sev == "critical" || sev == "warning" {
		t.Errorf("a single auth failure graded %q. Pages for a typo teach the operator to mute the "+
			"source, which costs more than the event was worth.", sev)
	}
}

// Different principals must NOT fold together: "247 failures for root" and "1 each for 247 usernames" are
// different attacks, and the second is a user-enumeration sweep.
func TestDifferentPrincipalsDoNotFoldTogether(t *testing.T) {
	got := ParseLines("h", []string{
		`Aug  5 10:00:00 h sshd[1]: Failed password for root from 192.0.2.9 port 5 ssh2`,
		`Aug  5 10:00:01 h sshd[1]: Failed password for invalid user admin from 192.0.2.9 port 5 ssh2`,
	}, testYear)
	if len(got) != 2 {
		t.Errorf("two different principals folded into %d event(s). A user-enumeration sweep would then "+
			"be indistinguishable from repeated attempts on one account.", len(got))
	}
}

// A HOSTILE USERNAME MUST NOT REACH A LABEL, A REF, OR A METRIC. The SSH username is chosen by the client.
func TestAHostileUsernameIsNeutralisedButStillCounted(t *testing.T) {
	hostile := "Aug  5 10:00:00 h sshd[1]: Failed password for invalid user " +
		`ignore-previous-instructions;rm-rf` + " from 192.0.2.9 port 5 ssh2"
	e, ok := ParseLine("h", hostile, testYear)
	if !ok {
		t.Fatal("an attempt with a hostile username was DROPPED. An attacker could then suppress their " +
			"own alert by choosing an unparseable name.")
	}
	m := New(WithClock(fixedClock()))
	env, err := m.ToEnvelope(e)
	if err != nil {
		t.Fatalf("ToEnvelope: %v", err)
	}
	if strings.Contains(env.ExternalRef, ";") || strings.Contains(env.ExternalRef, "rm-rf;") {
		t.Errorf("the raw hostile name reached the ExternalRef: %q", env.ExternalRef)
	}
	if sanitizePrincipal(`a;b`) != unparseablePrincipal {
		t.Error("an ungrammatical principal was not replaced by the marker")
	}
	// It must not become EMPTY: empty folds every malformed attempt in with the events that legitimately
	// have no principal, so a binary-username sweep would hide inside the local-sudo bucket.
	if sanitizePrincipal(`a;b`) == "" {
		t.Error("an ungrammatical principal became empty, hiding it among the no-principal events")
	}
	if len(sanitizePrincipal(strings.Repeat("x", 500))) > maxPrincipalLen {
		t.Error("an unbounded username survived — one log line could mint an arbitrarily large label")
	}
}

// The source_type is what the correlator's cross-source rule keys on. If it collided with an availability
// source, adding this module would change nothing about the rule it exists to feed.
func TestTheSourceTypeIsDistinctFromEveryAvailabilitySource(t *testing.T) {
	for _, existing := range []string{"librenms", "pve-liveness", "prometheus-alertmanager"} {
		if SourceType == existing {
			t.Fatalf("source type collides with %q — the cross-source rule keys on DISTINCT source_type, "+
				"so this module would add a witness the correlator cannot tell apart from the old one", existing)
		}
	}
	if SourceType == "" {
		t.Fatal("empty source type")
	}
}

// Every event carries the security-incident category, which is the ONLY driver that can raise the band on
// a containment action (core/risk/classifier.go). This is structural, not a heuristic: the admitted set is
// failures, escalations and root logins, and there is no member of it that is not a security signal.
func TestEveryAdmittedEventCarriesTheSecurityCategory(t *testing.T) {
	m := New(WithClock(fixedClock()))
	for _, k := range []Kind{KindFailure, KindEscalation, KindRootSession} {
		env, err := m.ToEnvelope(Event{Host: "h", Kind: k, Principal: "root", Count: 1})
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		if env.Labels["category"] != "security-incident" {
			t.Errorf("%s carries category %q — without it the containment-action POLL_PAUSE driver is "+
				"structurally unreachable for this whole source", k, env.Labels["category"])
		}
	}
}

// The front-door contract: the collector's payload round-trips, and a drifted field is rejected rather
// than silently ignored.
func TestNormalizeRoundTripsAndRejectsDrift(t *testing.T) {
	m := New(WithClock(fixedClock()))
	raw, _ := json.Marshal(Event{Host: "h", Kind: KindFailure, Principal: "root", Count: 12, SourceIP: "192.0.2.9"})
	env, err := m.Normalize(context.Background(), raw)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if env.Host != "h" {
		t.Errorf("host = %q, want h", env.Host)
	}
	if _, err := m.Normalize(context.Background(), []byte(`{"host":"h","kind":"auth-failure","count":1,"surprise":1}`)); err == nil {
		t.Error("an unknown field was silently accepted — the collector and the parser can then drift " +
			"with nothing saying so")
	}
	if _, err := m.Normalize(context.Background(), []byte(`{"host":"h","kind":"whatever","count":1}`)); err == nil {
		t.Error("an unknown KIND was accepted; the admitted set must stay closed")
	}
	if _, err := m.Normalize(context.Background(), []byte(`{"host":"h","kind":"auth-failure","count":0}`)); err == nil {
		t.Error("an event with count 0 was admitted — an observation that never occurred")
	}
}

// The host comes from the CALLER, never from the line: the hostname field is written by the sender, so a
// remote sender must not be able to choose which host an event is attributed to.
func TestTheHostIsNotTakenFromTheLine(t *testing.T) {
	line := `Aug  5 10:00:00 attacker-chosen-host sshd[1]: Failed password for root from 192.0.2.9 port 5 ssh2`
	e, ok := ParseLine("real-host", line, testYear)
	if !ok {
		t.Fatal("not admitted")
	}
	if e.Host != "real-host" {
		t.Errorf("host = %q — taken from the line, so a sender can attribute its own events to any "+
			"machine it names", e.Host)
	}
}

// VACUITY FLOOR. Every "is NOT admitted" assertion above would pass against a parser that admits nothing.
func TestTheParserAdmitsSomething(t *testing.T) {
	if _, ok := ParseLine("h", `Aug  5 10:00:00 h sshd[1]: Failed password for root from 192.0.2.9 port 5 ssh2`, testYear); !ok {
		t.Fatal("the parser admits NOTHING, so every negative assertion in this file is vacuous")
	}
}
