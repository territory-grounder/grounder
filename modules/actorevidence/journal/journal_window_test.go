package journal

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

func readSelf() (string, error) {
	b, err := os.ReadFile("journal.go")
	return string(b), err
}

// THE ARGV IS FIXED TEXT (TG-668 / TG-684, 2026-11-07). The host's read-only guard admits SSH_ORIGINAL_COMMAND by
// exact match against a file tools/readallow renders, so an argv that carries the window as two absolute
// timestamps can NEVER be admitted — and until this change every journal read was refused (exit 42) and then
// misread as a clean miss. The window is a relative --since; the caller's bound is applied client-side.
// KILLING MUTATION: put a formatted timestamp back into Argv. RED (and the guard e2e in tools/readallow goes red
// with it).
func TestTheArgvIsFixedTextTheGuardCanEnumerate(t *testing.T) {
	for _, ssh := range []bool{false, true} {
		argv := Argv(ssh)
		joined := strings.Join(argv, " ")
		if !strings.Contains(joined, "--since "+journalLookbackArg) {
			t.Errorf("Argv(%v) must carry the fixed relative lookback %q, got %q", ssh, journalLookbackArg, joined)
		}
		if strings.Contains(joined, "--until") {
			t.Errorf("Argv(%v) must not carry --until (the window is applied client-side), got %q", ssh, joined)
		}
		for _, a := range argv {
			if strings.ContainsAny(a, "0123456789") && a != journalLookbackArg {
				t.Errorf("Argv(%v) carries a variable-looking element %q — every element must be the same bytes on every call", ssh, a)
			}
		}
	}
	if a, b := Argv(true), Argv(true); strings.Join(a, " ") != strings.Join(b, " ") {
		t.Fatal("Argv must be identical across calls — that is what makes it enumerable")
	}
	if !strings.Contains(strings.Join(Argv(true), " "), "+ SYSLOG_IDENTIFIER=sshd") || strings.Contains(strings.Join(Argv(false), " "), "sshd") {
		t.Fatal("the sshd match group must appear exactly when sshSessions is on")
	}
	wire := WireCommands()
	if len(wire) != 2 || wire[0] != syslogng.RemoteCommand(Argv(false)) || wire[1] != syslogng.RemoteCommand(Argv(true)) {
		t.Fatalf("WireCommands must be both variants of Argv through the runner's own quoting, got %q", wire)
	}
	src, err := readSelf()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(src, `"--until"`) || strings.Contains(src, "journalStamp(") {
		t.Error("journal.go still builds a timestamped bound — the guard can never admit that shape")
	}
}

// Read sends EXACTLY Argv — not a copy that could drift from what readallow renders.
func TestReadSendsExactlyTheExportedArgv(t *testing.T) {
	fr := &fakeRunner{stdout: ""}
	m := New([]Access{{Site: "NL", HostGlob: "web01"}}, fr, fakeResolver{bundle: testBundle(t)}, WithSSHSessions(true))
	now := time.Now()
	if _, err := m.Read(context.Background(), "web01", now.Add(-30*time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fr.gotArgv, " ") != strings.Join(Argv(true), " ") {
		t.Fatalf("Read sent %q, Argv(true) is %q", fr.gotArgv, Argv(true))
	}
}

// The caller's window is applied CLIENT-SIDE: a row inside the 2h lookback but outside [since, until] is dropped,
// and a window that starts before the lookback is refused rather than silently truncated.
func TestTheWindowIsAppliedClientSideAndNeverTruncatedSilently(t *testing.T) {
	now := time.Now()
	since, until := now.Add(-30*time.Minute), now
	fr := &fakeRunner{stdout: journalLinesAt(now.Add(-90*time.Minute)) + journalLinesAt(now.Add(-10*time.Minute))}
	m := New([]Access{{Site: "NL", HostGlob: "web01"}}, fr, fakeResolver{bundle: testBundle(t)})
	out, err := m.Read(context.Background(), "web01", since, until)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range out {
		if e.ObservedAt.Before(since) || e.ObservedAt.After(until) {
			t.Fatalf("a row outside [since, until] leaked through the client-side filter: %+v", e)
		}
	}
	if len(out) == 0 {
		t.Fatal("the in-window rows must survive the filter")
	}
	if _, err := m.Read(context.Background(), "web01", now.Add(-3*time.Hour), now); err == nil || !strings.Contains(err.Error(), "lookback") {
		t.Fatalf("a window wider than the fixed lookback must be refused, got %v", err)
	}
	// A historical replay cannot be served by a relative lookback either: refused, never a clean miss.
	past := now.Add(-24 * time.Hour)
	if _, err := m.Read(context.Background(), "web01", past.Add(-30*time.Minute), past); err == nil || !strings.Contains(err.Error(), "historical") {
		t.Fatalf("a window that does not end now must be refused, got %v", err)
	}
}

// journalLinesAt renders one sudo COMMAND row stamped at the given instant.
func journalLinesAt(at time.Time) string {
	return `{"MESSAGE":"kp : TTY=pts/0 ; PWD=/root ; USER=root ; COMMAND=/usr/bin/systemctl restart nginx","_COMM":"sudo","SYSLOG_IDENTIFIER":"sudo","__REALTIME_TIMESTAMP":"` + itoa(at.UnixMicro()) + `","__CURSOR":"s=abc;i=` + itoa(at.Unix()) + `"}
`
}
