package k8saudit

// spec/023 T-023-9 — the k8s audit reader's oracles (REQ-2306/2307/2312/2304-half-2). The claims:
//
//   1. Deterministic parse: only completed MUTATING audit Events naming the subject, inside the window,
//      become Evidence — reads, other objects the coarse grep matched, other stages, out-of-window and
//      non-JSON lines are dropped, never guessed at.
//   2. INV-02 transport: the remote read is ONE fixed bounded argv; the validated subject travels as its
//      own element behind `--`; a metacharacter/dash-leading/traversal "target" refuses before any read.
//   3. Fail directions: unresolvable identity and runner errors are errors (advisory to the caller);
//      a clean read with no in-window mutation emits the affirmative COVERAGE MARKER, never silence.
//   4. Config parsing grants nothing on a malformed row (no wildcard, no relative/traversal path).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

type fakeAuditRunner struct {
	argv []string
	host string
	out  syslogng.RunResult
	err  error
}

func (f *fakeAuditRunner) Run(_ context.Context, s syslogng.Server, argv []string) (syslogng.RunResult, error) {
	f.argv = append([]string(nil), argv...)
	f.host = s.SSHHost
	return f.out, f.err
}

type fakeAuditResolver struct{ err error }

func (r fakeAuditResolver) Resolve(context.Context, credential.Target) (credential.Bundle, error) {
	if r.err != nil {
		return credential.Bundle{}, r.err
	}
	return credential.Bundle{}, nil
}

func auditLine(verb, stage, name, user, ts string) string {
	return fmt.Sprintf(`{"kind":"Event","apiVersion":"audit.k8s.io/v1","auditID":"aid-%s-%s","stage":%q,"verb":%q,"user":{"username":%q},"objectRef":{"resource":"deployments","namespace":"web","name":%q},"requestReceivedTimestamp":%q}`,
		verb, name, stage, verb, user, name, ts)
}

func auditFixture(t *testing.T) (*Module, *fakeAuditRunner) {
	t.Helper()
	run := &fakeAuditRunner{}
	m := New(
		[]Access{{Site: "nl", Host: "dc1k8s01", Path: "/var/log/kubernetes/audit/audit.log"}},
		run, fakeAuditResolver{},
	)
	return m, run
}

var auditWindow = struct{ since, until time.Time }{
	since: time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC),
	until: time.Date(2026, 8, 23, 1, 0, 0, 0, time.UTC),
}

func TestAuditParseIsDeterministicAndMutatingOnly(t *testing.T) {
	m, run := auditFixture(t)
	run.out = syslogng.RunResult{Stdout: []byte(strings.Join([]string{
		auditLine("patch", "ResponseComplete", "frontend", "admin@example", "2026-08-23T00:10:00Z"),         // KEPT
		auditLine("get", "ResponseComplete", "frontend", "reader@example", "2026-08-23T00:11:00Z"),          // read — dropped
		auditLine("patch", "RequestReceived", "frontend", "early@example", "2026-08-23T00:12:00Z"),          // wrong stage — dropped
		auditLine("delete", "ResponseComplete", "frontend-canary", "other@example", "2026-08-23T00:13:00Z"), // other object the grep coarse-matched — dropped
		auditLine("update", "ResponseComplete", "frontend", "late@example", "2026-08-23T02:00:00Z"),         // out of window — dropped
		"not json at all",
		auditLine("delete", "ResponseComplete", "frontend", "system:serviceaccount:kube-system:gc", "2026-08-23T00:40:00Z"), // KEPT
	}, "\n"))}
	evs, err := m.Read(context.Background(), "frontend", auditWindow.since, auditWindow.until)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("want exactly the 2 in-window completed mutations, got %d: %+v", len(evs), evs)
	}
	if evs[0].Actor != "admin@example" || evs[0].ActionKind != "patch:deployments/web" || !evs[0].Covered {
		t.Fatalf("first evidence wrong: %+v", evs[0])
	}
	if evs[1].Actor != "system:serviceaccount:kube-system:gc" || evs[1].Ref != "aid-delete-frontend" {
		t.Fatalf("second evidence wrong: %+v", evs[1])
	}
	if evs[0].Domain != "k8s-audit" || evs[0].Target != "frontend" {
		t.Fatalf("evidence must carry the domain and the subject, got %+v", evs[0])
	}
}

func TestAuditTransportIsOneFixedBoundedArgv(t *testing.T) {
	m, run := auditFixture(t)
	run.out = syslogng.RunResult{ExitCode: 1} // clean miss
	if _, err := m.Read(context.Background(), "frontend", auditWindow.since, auditWindow.until); err != nil {
		t.Fatalf("clean miss must not error: %v", err)
	}
	want := []string{"grep", "-m", "2000", "-F", "--", "frontend", "/var/log/kubernetes/audit/audit.log"}
	if len(run.argv) != len(want) {
		t.Fatalf("argv %v, want %v", run.argv, want)
	}
	for i := range want {
		if run.argv[i] != want[i] {
			t.Fatalf("argv[%d]=%q want %q (full %v)", i, run.argv[i], want[i], run.argv)
		}
	}
	if run.host != "dc1k8s01" {
		t.Fatalf("the read must go to the DECLARED control plane, got %q", run.host)
	}
	// A hostile "target" never reaches the runner: metacharacters, a leading dash (flag injection into
	// grep), and traversal all refuse structurally.
	for _, bad := range []string{"web;rm -rf /", "-e", "--", "a b", "../etc/passwd", ""} {
		run.argv = nil
		if _, err := m.Read(context.Background(), bad, auditWindow.since, auditWindow.until); err == nil {
			t.Errorf("target %q must refuse", bad)
		}
		if run.argv != nil {
			t.Errorf("target %q reached the runner: %v", bad, run.argv)
		}
	}
}

func TestAuditFailDirectionsAndCoverage(t *testing.T) {
	// Unresolvable identity → an error (advisory to the caller), the runner never consulted.
	run := &fakeAuditRunner{}
	m := New([]Access{{Site: "nl", Host: "cp1", Path: "/var/log/audit.log"}}, run, fakeAuditResolver{err: credential.ErrUnresolved})
	if _, err := m.Read(context.Background(), "frontend", auditWindow.since, auditWindow.until); err == nil {
		t.Fatal("an unresolvable identity must surface as an error, not silence")
	}
	if run.argv != nil {
		t.Fatal("no credential ⇒ no read")
	}
	// Runner error → error.
	m2, run2 := auditFixture(t)
	run2.err = errors.New("connection refused")
	if _, err := m2.Read(context.Background(), "frontend", auditWindow.since, auditWindow.until); err == nil {
		t.Fatal("a transport failure must surface as an error")
	}
	// Clean miss → the affirmative coverage marker, never an empty slice (REQ-2304 half 2).
	m3, run3 := auditFixture(t)
	run3.out = syslogng.RunResult{ExitCode: 1}
	evs, err := m3.Read(context.Background(), "frontend", auditWindow.since, auditWindow.until)
	if err != nil || len(evs) != 1 || !evs[0].Covered || evs[0].Actor != "" {
		t.Fatalf("a clean miss must emit exactly the coverage marker, got %+v err=%v", evs, err)
	}
	// No declared plane / not configured → errors.
	if _, err := New(nil, run, fakeAuditResolver{}).Read(context.Background(), "frontend", auditWindow.since, auditWindow.until); err == nil {
		t.Fatal("no declared control plane must refuse")
	}
}

func TestAuditParseAccessGrantsNothingOnMalformedRows(t *testing.T) {
	got := ParseAccess("nl|dc1k8s01|/var/log/kubernetes/audit/audit.log;" + // valid
		"gr|dc2k8s01|relative/path;" + // not absolute — dropped
		"nl|cp2|/var/log/../etc/shadow;" + // traversal — dropped
		"nl||/var/log/a.log;" + // no host — dropped
		"nl|cp3|/var/log/x.log$(id);" + // metacharacters — dropped
		"just-garbage")
	if len(got) != 1 || got[0].Host != "dc1k8s01" {
		t.Fatalf("only the valid row may grant access, got %+v", got)
	}
}
