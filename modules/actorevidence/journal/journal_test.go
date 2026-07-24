package journal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// fakeRunner returns a fixed RunResult (or an error) for any argv — the syslogng transport is exercised
// separately; here we test the reader's parse + gate logic.
type fakeRunner struct {
	stdout   string
	exitCode int
	err      error
	gotArgv  []string
}

func (f *fakeRunner) Run(_ context.Context, _ syslogng.Server, argv []string) (syslogng.RunResult, error) {
	f.gotArgv = argv
	if f.err != nil {
		return syslogng.RunResult{}, f.err
	}
	return syslogng.RunResult{Stdout: []byte(f.stdout), ExitCode: f.exitCode}, nil
}

// fakeResolver returns a fixed bundle (or a fail-closed refusal).
type fakeResolver struct {
	bundle credential.Bundle
	err    error
}

func (f fakeResolver) Resolve(_ context.Context, _ credential.Target) (credential.Bundle, error) {
	return f.bundle, f.err
}

func testBundle(t *testing.T) credential.Bundle {
	t.Helper()
	b, err := credential.NewBundle(credential.BundleSpec{
		User: "tg-ro", Port: 22, Scheme: credential.SchemeSSH, SSHKeyRef: config.SecretRef("env:JOURNAL_RO_KEY"),
	})
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	return b
}

// Real `journalctl -o json` lines (field shape captured from a live host 2026-07-24): a COMMAND event
// naming the invoker "kp", a session-open naming "alice", a session-close naming nobody (skipped), and a
// non-sudo line (skipped). Timestamps are __REALTIME_TIMESTAMP microseconds.
func journalLines(base time.Time) string {
	us := func(offsetSec int) string {
		return itoa(base.Add(time.Duration(offsetSec) * time.Second).UnixMicro())
	}
	return `{"MESSAGE":"kp : TTY=pts/0 ; PWD=/root ; USER=root ; COMMAND=/usr/bin/systemctl restart nginx","_COMM":"sudo","_AUDIT_LOGINUID":"1001","SYSLOG_IDENTIFIER":"sudo","__REALTIME_TIMESTAMP":"` + us(-300) + `","__CURSOR":"s=abc;i=1"}
{"MESSAGE":"pam_unix(sudo:session): session opened for user root by alice(uid=1002)","_COMM":"sudo","_AUDIT_LOGINUID":"1002","SYSLOG_IDENTIFIER":"sudo","__REALTIME_TIMESTAMP":"` + us(-200) + `","__CURSOR":"s=abc;i=2"}
{"MESSAGE":"pam_unix(sudo:session): session closed for user root","_COMM":"sudo","_AUDIT_LOGINUID":"0","SYSLOG_IDENTIFIER":"sudo","__REALTIME_TIMESTAMP":"` + us(-100) + `","__CURSOR":"s=abc;i=3"}
{"MESSAGE":"Started something","_COMM":"systemd","SYSLOG_IDENTIFIER":"systemd","__REALTIME_TIMESTAMP":"` + us(-50) + `","__CURSOR":"s=abc;i=4"}`
}

func TestReadParsesSudoActorEvidence(t *testing.T) {
	base := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	fr := &fakeRunner{stdout: journalLines(base)}
	m := New([]Access{{Site: "NL", HostGlob: "web01"}}, fr, fakeResolver{bundle: testBundle(t)})
	out, err := m.Read(context.Background(), "web01", base.Add(-30*time.Minute), base)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// The COMMAND (kp) and session-open (alice) name a principal; the session-close names nobody, and the
	// non-sudo systemd line is not sudo — both are skipped.
	if len(out) != 2 {
		t.Fatalf("want 2 principal-naming sudo records, got %d: %+v", len(out), out)
	}
	if out[0].Actor != "kp" || out[0].ActionKind != "sudo-command" || out[0].Domain != "journal" || out[0].Target != "web01" || !out[0].Covered {
		t.Fatalf("the command row must be journal/kp/sudo-command/covered on web01, got %+v", out[0])
	}
	if out[0].Ref == "" || out[0].ObservedAt.IsZero() {
		t.Fatalf("the command row must carry the journal cursor as Ref and a real timestamp, got %+v", out[0])
	}
	if out[1].Actor != "alice" || out[1].ActionKind != "sudo-session-open" {
		t.Fatalf("the session-open row must name alice, got %+v", out[1])
	}
	// The FIXED argv must carry -o json, the window, and the sudo filter — never model text.
	joined := ""
	for _, a := range fr.gotArgv {
		joined += a + " "
	}
	if !contains(fr.gotArgv, "journalctl") || !contains(fr.gotArgv, "SYSLOG_IDENTIFIER=sudo") || !contains(fr.gotArgv, "json") {
		t.Fatalf("argv must be the fixed journalctl -o json sudo read, got %q", joined)
	}
}

func TestReadRefusesHostNotInAllowlist(t *testing.T) {
	fr := &fakeRunner{stdout: ""}
	m := New([]Access{{Site: "NL", HostGlob: "web01"}}, fr, fakeResolver{bundle: testBundle(t)})
	if _, err := m.Read(context.Background(), "db99", time.Now().Add(-time.Hour), time.Now()); err == nil {
		t.Fatal("a host outside the allowlist must be refused (advisory error), got nil")
	}
	if fr.gotArgv != nil {
		t.Fatal("an unallowed host must never reach the runner")
	}
}

func TestReadDegradesOnUnresolvableCredential(t *testing.T) {
	m := New([]Access{{Site: "NL", HostGlob: "*"}}, &fakeRunner{}, fakeResolver{err: credential.ErrUnresolved})
	if _, err := m.Read(context.Background(), "web01", time.Now().Add(-time.Hour), time.Now()); err == nil {
		t.Fatal("an unresolvable credential must degrade to an advisory error (REQ-2307), got nil")
	}
}

func TestReadDegradesOnRunnerError(t *testing.T) {
	m := New([]Access{{Site: "NL", HostGlob: "*"}}, &fakeRunner{err: errors.New("dial refused")}, fakeResolver{bundle: testBundle(t)})
	if _, err := m.Read(context.Background(), "web01", time.Now().Add(-time.Hour), time.Now()); err == nil {
		t.Fatal("a runner error must be advisory (REQ-2307), got nil")
	}
}

// Evidence OUTSIDE the window is dropped by the reader's own re-check (defense in depth), and a record with
// no principal and no audit login uid is dropped.
func TestReadDropsOutOfWindowAndActorlessRecords(t *testing.T) {
	base := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	lines := `{"MESSAGE":"kp : TTY=pts/0 ; COMMAND=/bin/x","_COMM":"sudo","SYSLOG_IDENTIFIER":"sudo","__REALTIME_TIMESTAMP":"` + itoa(base.Add(-2*time.Hour).UnixMicro()) + `","__CURSOR":"s=a;i=9"}
{"MESSAGE":"pam_unix(sudo:session): session closed for user root","_COMM":"sudo","SYSLOG_IDENTIFIER":"sudo","__REALTIME_TIMESTAMP":"` + itoa(base.Add(-5*time.Minute).UnixMicro()) + `","__CURSOR":"s=a;i=10"}`
	m := New([]Access{{Site: "NL", HostGlob: "*"}}, &fakeRunner{stdout: lines}, fakeResolver{bundle: testBundle(t)})
	out, err := m.Read(context.Background(), "web01", base.Add(-30*time.Minute), base)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("an out-of-window row and an actorless session-close must both be dropped, got %+v", out)
	}
}

func TestParseAccessSkipsMalformedRows(t *testing.T) {
	accs := ParseAccess("NL|web01|root|env:K; ;GR|*db*|root|env:K2; badrow")
	if len(accs) != 2 {
		t.Fatalf("want 2 valid access rows, got %d: %+v", len(accs), accs)
	}
	if accs[0].HostGlob != "web01" || accs[1].HostGlob != "*db*" {
		t.Fatalf("access rows parsed wrong: %+v", accs)
	}
}

func TestSudoInvoker(t *testing.T) {
	cases := map[string]string{
		"kp : TTY=pts/0 ; PWD=/ ; USER=root ; COMMAND=/bin/x":                     "kp",
		"pam_unix(sudo:session): session opened for user root by alice(uid=1002)": "alice",
		"pam_unix(sudo:session): session closed for user root":                    "",
		"": "",
		// LDAP/Kerberos realm form + machine account must parse (the identity seam resolves these).
		"alice@SEC.NUCLEARLIGHTERS.NET : TTY=pts/0 ; COMMAND=/bin/x":                      "alice@SEC.NUCLEARLIGHTERS.NET",
		"pam_unix(sudo:session): session opened for user root by bob@sec.realm(uid=1500)": "bob@sec.realm",
	}
	for msg, want := range cases {
		if got := sudoInvoker(msg); got != want {
			t.Fatalf("sudoInvoker(%q) = %q, want %q", msg, got, want)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

func itoa(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
