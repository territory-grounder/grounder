package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// probeRunner is the transport seam the production runner also sits behind (WithRunner). It records every
// argv it was handed, so the read-only claim is asserted rather than assumed, and it can fail a chosen host
// with a chosen error — which is how the SSH failure shapes below are reproduced without an SSH server.
type probeRunner struct {
	mu    sync.Mutex
	out   map[string][]byte
	err   map[string]error
	block map[string]chan struct{}
	seen  [][]string
	hosts []string
}

func (p *probeRunner) Run(ctx context.Context, host string, argv []string) ([]byte, error) {
	p.mu.Lock()
	p.seen = append(p.seen, argv)
	p.hosts = append(p.hosts, host)
	ch := p.block[host]
	p.mu.Unlock()
	if ch != nil {
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if e, ok := p.err[host]; ok {
		return nil, e
	}
	return p.out[host], nil
}

// TestSelfTestAcrossDeclaredHosts is the table over what an operator actually hits: healthy hosts, a host
// with no docker, and each layer of the read-only transport refusing in turn (allowlist, credential engine,
// host-key verification, authentication, reachability). Each case asserts the DETAIL names the layer that
// refused, because "error" leaves the operator guessing which of five things to fix.
func TestSelfTestAcrossDeclaredHosts(t *testing.T) {
	cases := []struct {
		name            string
		hosts           []string
		runner          *probeRunner
		wantErr         bool
		summaryContains []string
		detailContains  string
	}{
		{
			name:  "healthy hosts report the containers they returned",
			hosts: []string{"dc1actualbudget01", "dc1mealie01"},
			runner: &probeRunner{out: map[string][]byte{
				"dc1actualbudget01": []byte("actualbudget-actual_server-1\nnginx-proxy\n"),
				"dc1mealie01":       []byte("mealie\n"),
			}},
			// Every number comes from the SERVED output, not from configuration — that is what lets a green
			// Test reveal a host list pointed at the wrong machines.
			summaryContains: []string{
				"2 of 2 declared hosts", "3 containers",
				"dc1actualbudget01 (2)", "dc1mealie01 (1)",
				"actualbudget-actual_server-1",
			},
		},
		{
			name:  "a host with no docker is a result, not a failure",
			hosts: []string{"dc1bare01"},
			// The transport treats a non-zero remote exit as "nothing here", so this is what a machine
			// without docker looks like: an empty read that succeeded.
			runner:          &probeRunner{out: map[string][]byte{"dc1bare01": []byte("")}},
			summaryContains: []string{"1 of 1 declared hosts", "0 containers"},
			detailContains:  "RESULT, not a failure",
		},
		{
			name:  "an unverifiable host key is named as a host-key problem",
			hosts: []string{"dc1new01"},
			runner: &probeRunner{err: map[string]error{
				"dc1new01": errors.New("discovery: dc1new01: ssh: handshake failed: knownhosts: key is unknown"),
			}},
			wantErr:        true,
			detailContains: "known_hosts",
		},
		{
			name:  "a CHANGED host key is called what it is",
			hosts: []string{"dc1db01"},
			runner: &probeRunner{err: map[string]error{
				"dc1db01": errors.New("discovery: dc1db01: ssh: handshake failed: knownhosts: key mismatch"),
			}},
			wantErr:        true,
			detailContains: "security event",
		},
		{
			name:  "a rejected key is an account problem, not a host-key one",
			hosts: []string{"dc1app01"},
			runner: &probeRunner{err: map[string]error{
				"dc1app01": errors.New("discovery: dc1app01: ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey]"),
			}},
			wantErr:        true,
			detailContains: "authorized_keys",
		},
		{
			name:  "no credential for a host is reported as fail-closed, not as a host fault",
			hosts: []string{"dc1unknown01"},
			runner: &probeRunner{err: map[string]error{
				"dc1unknown01": errors.New(`discovery: no resolvable read-only SSH credential for "dc1unknown01" (fail closed): not found`),
			}},
			wantErr:        true,
			detailContains: "audited credential engine",
		},
		{
			name:  "a host outside the allowlist says the transport refused it",
			hosts: []string{"dc1stale01"},
			runner: &probeRunner{err: map[string]error{
				"dc1stale01": errors.New(`discovery: host "dc1stale01" is not in the operator discovery allowlist (fail closed)`),
			}},
			wantErr:        true,
			detailContains: "read once at boot",
		},
		{
			name:  "an unreachable host is unreachable, and the others still contribute",
			hosts: []string{"dc1good01", "dc1down01"},
			runner: &probeRunner{
				out: map[string][]byte{"dc1good01": []byte("app-1\n")},
				err: map[string]error{"dc1down01": errors.New("discovery: dc1down01: dial tcp 192.0.2.9:22: connect: connection refused")},
			},
			wantErr:         true,
			summaryContains: []string{"1 of 2 declared hosts", "1 container", "dc1good01 (1)"},
			detailContains:  "could not be reached",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := New(tc.hosts, WithRunner(tc.runner)).SelfTest(context.Background(), "alice")
			if tc.wantErr != (err != nil) {
				t.Fatalf("wantErr=%v, got err=%v (result %+v)", tc.wantErr, err, res)
			}
			for _, want := range tc.summaryContains {
				if !strings.Contains(res.Summary, want) {
					t.Errorf("Summary must report what was observed; %q missing from %q", want, res.Summary)
				}
			}
			if tc.detailContains != "" && !strings.Contains(res.Detail, tc.detailContains) {
				t.Errorf("Detail must be actionable; %q missing from %q", tc.detailContains, res.Detail)
			}
			// The failing host must be NAMED, or the operator has to guess which of their machines broke.
			for h, e := range tc.runner.err {
				if e != nil && !strings.Contains(res.Summary+res.Detail, h) {
					t.Errorf("the failing host %q must be named in the result, got %q / %q", h, res.Summary, res.Detail)
				}
			}
		})
	}
}

// TestSelfTestIssuesOnlyTheFixedReadOnlyEnumeration pins the read-only property at the level that matters:
// the probe issues the package's own `docker ps` CONSTANT, once per host, and never builds a command. A
// discovery probe that could construct argv would be an execution path reachable from a settings dialog.
func TestSelfTestIssuesOnlyTheFixedReadOnlyEnumeration(t *testing.T) {
	r := &probeRunner{out: map[string][]byte{"h1": []byte("a\n"), "h2": []byte("b\n")}}
	if _, err := New([]string{"h1", "h2"}, WithRunner(r)).SelfTest(context.Background(), "alice"); err != nil {
		t.Fatalf("SelfTest must succeed: %v", err)
	}
	if len(r.seen) != 2 {
		t.Fatalf("exactly one read per host expected, got %d", len(r.seen))
	}
	for _, argv := range r.seen {
		if strings.Join(argv, " ") != strings.Join(listContainersArgv, " ") {
			t.Fatalf("the probe must issue the package's fixed enumeration, got %v", argv)
		}
		for _, a := range argv {
			if a == "restart" || a == "stop" || a == "kill" || a == "rm" || a == "run" || a == "exec" {
				t.Fatalf("the probe issued a MUTATING docker verb: %v", argv)
			}
		}
	}
}

// TestSelfTestIsNotAConfigurationCheck IS THE KILLING ORACLE.
//
// The configuration is complete and valid — a non-empty host list, a wired runner — and every host refuses
// the credential. A SelfTest implemented as "the settings are filled in" passes this; the real probe cannot,
// because it has to hear the hosts answer. This is what separates a probe from a mock wearing a test's name,
// and it is why a key that was rotated out of authorized_keys cannot be certified green from a dialog.
func TestSelfTestIsNotAConfigurationCheck(t *testing.T) {
	hosts := []string{"dc1app01", "dc1db01"}
	r := &probeRunner{err: map[string]error{
		"dc1app01": errors.New("discovery: dc1app01: ssh: handshake failed: ssh: unable to authenticate"),
		"dc1db01":  errors.New("discovery: dc1db01: ssh: handshake failed: ssh: unable to authenticate"),
	}}
	src := New(hosts, WithRunner(r))
	if len(src.hosts) != 2 || src.run == nil {
		t.Fatal("the fixture must have COMPLETE configuration, or this oracle proves nothing")
	}
	res, err := src.SelfTest(context.Background(), "alice")
	if err == nil {
		t.Fatalf("hosts that refuse the credential must FAIL the test, got a pass: %+v", res)
	}
	if !strings.Contains(res.Summary, "could not run `docker ps` on any") {
		t.Errorf("Summary must say nothing was read, got %q", res.Summary)
	}
}

// TestSelfTestWithNoRunnerFailsInsteadOfPassingVacuously — a source with no transport has read nothing, and
// "nothing to report" must never render as a pass. This is the same failure the notifier probe guards with
// its nil-sink check.
func TestSelfTestWithNoRunnerFailsInsteadOfPassingVacuously(t *testing.T) {
	res, err := New([]string{"dc1app01"}).SelfTest(context.Background(), "alice")
	if err == nil {
		t.Fatalf("a source with no runner must fail the test, got %+v", res)
	}
	if !strings.Contains(res.Detail, "TG-side fault") {
		t.Errorf("Detail must place the fault on the TG side, got %q", res.Detail)
	}
}

// TestSelfTestRespectsTheActivityBudget — moduletest bounds the probe at 30 seconds with ONE attempt, so an
// unresponsive host must end the probe rather than the activity, and the hosts that never got their turn
// must be reported as NOT ATTEMPTED. Counting an unprobed host as reachable would be the worst possible
// answer: it certifies a machine nobody contacted.
func TestSelfTestRespectsTheActivityBudget(t *testing.T) {
	hosts := make([]string, 0, selfTestFanout+2)
	blocked := map[string]chan struct{}{}
	for i := 0; i < cap(hosts); i++ {
		h := fmt.Sprintf("dc1host%02d", i)
		hosts = append(hosts, h)
		blocked[h] = make(chan struct{}) // never released: every host hangs until the context expires
	}
	r := &probeRunner{block: blocked}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := New(hosts, WithRunner(r)).SelfTest(ctx, "alice")
	if err == nil {
		t.Fatalf("hosts that never answer must fail the test, got %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the probe must end with the context, took %s", elapsed)
	}
	if !strings.Contains(res.Detail, "not attempted") && !strings.Contains(res.Detail, "timed out") {
		t.Errorf("Detail must distinguish hosts that failed from hosts never tried, got %q", res.Detail)
	}
}

// TestSelfTestBoundsWhatItReports — the host list may hold 64 entries. The totals stay exact; only the
// enumeration is trimmed, because a Result is rendered in a dialog and pasted into tickets.
func TestSelfTestBoundsWhatItReports(t *testing.T) {
	out := map[string][]byte{}
	hosts := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		h := fmt.Sprintf("dc1host%02d", i)
		hosts = append(hosts, h)
		out[h] = []byte("app-1\n")
	}
	res, err := New(hosts, WithRunner(&probeRunner{out: out})).SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("SelfTest must succeed: %v", err)
	}
	if !strings.Contains(res.Summary, "20 of 20 declared hosts") || !strings.Contains(res.Summary, "20 containers") {
		t.Errorf("the totals must be exact, got %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "more") {
		t.Errorf("the per-host list must be truncated with a count of the remainder, got %q", res.Summary)
	}
	if n := strings.Count(res.Summary, "dc1host"); n > selfTestHostSample+1 { // +1: the example container's host
		t.Errorf("at most %d hosts may be named, %d were: %q", selfTestHostSample, n, res.Summary)
	}
}
