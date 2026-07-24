package ssh

import (
	"context"
	"strings"
	"testing"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
)

type fakeRunner struct{ argv []string }

func (f *fakeRunner) Run(_ context.Context, argv []string, _ []byte) (actuation.Result, error) {
	f.argv = argv
	return actuation.Result{ExitCode: 0}, nil
}

func TestExecWrapsFixedArgvNoShellNoBypass(t *testing.T) {
	f := &fakeRunner{}
	m := New("web01", "svc-agent", f)
	if m.Capability() != "ssh" || !m.ReadOnly() {
		t.Fatalf("capability/read-only wrong: %q %v", m.Capability(), m.ReadOnly())
	}
	if _, err := m.Exec(context.Background(), []string{"systemctl", "restart", "nginx"}, nil); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.argv, " ")
	if f.argv[0] != "ssh" {
		t.Errorf("must invoke ssh, got %q", f.argv[0])
	}
	if !strings.Contains(got, "StrictHostKeyChecking=yes") {
		t.Errorf("must verify host keys, got %q", got)
	}
	for _, forbidden := range []string{"StrictHostKeyChecking=no", "-t", "sh -c"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("must not express %q, got %q", forbidden, got)
		}
	}
	// the remote command is a single POSIX-shell-quoted word, so the remote login shell re-parses it back
	// into exactly the intended arguments.
	if f.argv[len(f.argv)-1] != `'systemctl' 'restart' 'nginx'` {
		t.Errorf("remote command must be shell-quoted per argument, got %q", f.argv[len(f.argv)-1])
	}
}

// A remote argument carrying shell metacharacters or spaces must be QUOTED, not passed raw — the ssh client
// space-joins the words after the host and the remote sshd runs them through the login shell, so a bare argv
// would inject / word-split. This is the INV-02 command-injection class.
func TestRemoteArgumentsAreShellQuoted(t *testing.T) {
	f := &fakeRunner{}
	m := New("web01", "svc-agent", f)
	if _, err := m.Exec(context.Background(), []string{"echo", "safe; touch /tmp/PWNED", "$(id -u)", "one two three", "it's"}, nil); err != nil {
		t.Fatal(err)
	}
	remote := f.argv[len(f.argv)-1]
	// Every element is wrapped in single quotes (embedded ' escaped as '\''), so the remote shell parses the
	// string back into exactly these five arguments — the `;`, `$(...)`, and spaces are all inert data.
	want := `'echo' 'safe; touch /tmp/PWNED' '$(id -u)' 'one two three' 'it'\''s'`
	if remote != want {
		t.Fatalf("remote command not safely quoted:\n got %q\nwant %q", remote, want)
	}
}

func TestEmptyArgvRejected(t *testing.T) {
	m := New("web01", "svc", &fakeRunner{})
	if _, err := m.Exec(context.Background(), nil, nil); err != actuation.ErrEmptyArgv {
		t.Fatalf("empty argv must be rejected, got %v", err)
	}
}

// ActuationHost exposes the single host every mutation of this leaf lands on, so the interceptor's host-match
// gate can refuse a target mismatch (this leaf runs the argv on its configured host, not the action's target).
func TestActuationHostReportsConfiguredHost(t *testing.T) {
	if got := New("web01", "svc", &fakeRunner{}).ActuationHost(); got != "web01" {
		t.Fatalf("ActuationHost must report the configured host, got %q", got)
	}
}

func TestRecordExecBindsRollbackToActionID(t *testing.T) {
	m := New("web01", "svc", &fakeRunner{})
	log, err := m.RecordExec("act-123", []string{"systemctl", "stop", "nginx"}, []string{"systemctl", "start", "nginx"})
	if err != nil {
		t.Fatal(err)
	}
	if log.ActionID != "act-123" || len(log.Rollback) == 0 {
		t.Errorf("execution_log must bind rollback to the action id: %+v", log)
	}
	if _, err := m.RecordExec("", []string{"a"}, []string{"b"}); err == nil {
		t.Error("an execution_log with no action id must be rejected")
	}
	if _, err := m.RecordExec("act-1", []string{"a"}, nil); err == nil {
		t.Error("a mutating command with no rollback must be rejected")
	}
}
