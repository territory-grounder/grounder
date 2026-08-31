package syslogng

// Oracles for the console's TEST button on the syslog-ng connector.
//
// These drive SelfTest against a REAL x/crypto SSH server served over a loopback pipe — the same in-process
// construction the native-runner oracles use (native_test.go) — so the client handshake, the known_hosts
// host-key verification and the in-memory key auth all run for real, with no network and no live syslog
// server. The fakeRunner the tool oracles use is the right seam for asserting remote argv; it is the wrong
// seam here, because what this probe claims to establish IS the handshake.
//
// The property asserted on EVERY path, passing and failing alike, is that the servers received NO exec
// request. The descriptor promises the session is opened and closed with nothing run on the operator's
// machine, and a probe that ran `true` on its way to reporting a pass would have broken that promise
// invisibly.

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const (
	nlHost = "dc1syslogng01"
	grHost = "dc2syslogng01"
)

// probeFixture is one in-process syslog server: a real SSH server on a loopback pipe, plus the channel that
// records any exec it is asked to run (which must stay empty for the life of the test).
type probeFixture struct {
	host       string
	hostSigner ssh.Signer
	clientEnd  net.Conn
	gotCmd     chan string
}

// serveProbeServer starts a real SSH server that accepts acceptPub and records any exec request.
func serveProbeServer(t *testing.T, host string, acceptPub ssh.PublicKey) *probeFixture {
	t.Helper()
	hostSigner, _ := genSigner(t)
	clientEnd, serverEnd := loopbackPipe(t)
	f := &probeFixture{host: host, hostSigner: hostSigner, clientEnd: clientEnd, gotCmd: make(chan string, 1)}
	go serveOneSSH(t, serverEnd, hostSigner, acceptPub, "no log may ever be read by a test\n", f.gotCmd)
	return f
}

// assertRanNothing is the read-only oracle: the descriptor's verb says the session is opened and closed with
// no command run, and this is what makes that literally checkable.
func (f *probeFixture) assertRanNothing(t *testing.T) {
	t.Helper()
	select {
	case cmd := <-f.gotCmd:
		t.Fatalf("the probe executed %q on %s — a TEST must run nothing on the operator's syslog server", cmd, f.host)
	default:
	}
}

// writeProbeKnownHosts writes a multi-entry OpenSSH known_hosts file (the real format is multi-host by
// design: one file covers every configured server).
func writeProbeKnownHosts(t *testing.T, pinned map[string]ssh.PublicKey) string {
	t.Helper()
	var b strings.Builder
	for host, pub := range pinned {
		b.WriteString(knownhosts.Line([]string{net.JoinHostPort(host, sshPort)}, pub) + "\n")
	}
	p := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	return p
}

// probeRunner builds the production native runner wired to the in-process servers by address.
func probeRunner(knownHosts string, fixtures ...*probeFixture) *nativeRunner {
	byAddr := map[string]net.Conn{}
	for _, f := range fixtures {
		byAddr[net.JoinHostPort(f.host, sshPort)] = f.clientEnd
	}
	return &nativeRunner{
		knownHosts:     knownHosts,
		connectTimeout: 5 * time.Second,
		dial: func(_ context.Context, addr string) (net.Conn, error) {
			c, ok := byAddr[addr]
			if !ok {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
			}
			return c, nil
		},
	}
}

func probeServers() []Server {
	return ParseServers("NL|" + nlHost + "|root|env:TG_TEST_SYSLOGNG_PROBE_KEY|/mnt/logs/syslog-ng;" +
		"GR|" + grHost + "|svc|env:TG_TEST_SYSLOGNG_PROBE_KEY|/mnt/logs/syslog-ng")
}

// THE HAPPY PATH, and the promise the descriptor makes: a session on every server, opened and closed, with
// nothing run.
func TestSelfTestOpensAndClosesASessionOnEveryServerRunningNothing(t *testing.T) {
	clientSigner, clientPEM := genSigner(t)
	t.Setenv("TG_TEST_SYSLOGNG_PROBE_KEY", string(clientPEM))

	nl := serveProbeServer(t, nlHost, clientSigner.PublicKey())
	gr := serveProbeServer(t, grHost, clientSigner.PublicKey())
	kh := writeProbeKnownHosts(t, map[string]ssh.PublicKey{
		nlHost: nl.hostSigner.PublicKey(),
		grHost: gr.hostSigner.PublicKey(),
	})
	m := &Module{servers: probeServers(), runner: probeRunner(kh, nl, gr)}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := m.SelfTest(ctx, "alice@example")
	if err != nil {
		t.Fatalf("both servers answer, so the test must pass: %v (detail: %s)", err, res.Detail)
	}
	// The observation names each server the way the operator configured it, so a pass against a row pointed
	// at the WRONG machine or carrying the wrong ssh user is still legible.
	for _, want := range []string{"all 2 configured syslog server(s)", "root@" + nlHost + " [NL]", "svc@" + grHost + " [GR]", "no command was run"} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("Summary %q must name %q", res.Summary, want)
		}
	}
	if !strings.Contains(res.Detail, "does NOT prove a log can be read") {
		t.Errorf("Detail must state the ceiling of the proof, got %q", res.Detail)
	}
	nl.assertRanNothing(t)
	gr.assertRanNothing(t)
}

// THE KILLING ORACLE.
//
// Every configured value is present and non-empty — site, ssh host, ssh user, a key reference that resolves
// to a real, parseable private key, and a known_hosts file that exists and pins this exact server — and the
// server refuses that key. A "the configuration is filled in" check returns a green here; this must return an
// error, because a key the syslog server's authorized_keys does not carry is one of the three things pressing
// TEST is meant to rule out, and it is invisible until something authenticates.
func TestSelfTestFailsWhenTheServerRefusesTheKeyDespiteCompleteConfig(t *testing.T) {
	_, clientPEM := genSigner(t)
	t.Setenv("TG_TEST_SYSLOGNG_PROBE_KEY", string(clientPEM))
	otherSigner, _ := genSigner(t) // the server's authorized_keys carries a DIFFERENT key

	nl := serveProbeServer(t, nlHost, otherSigner.PublicKey())
	kh := writeProbeKnownHosts(t, map[string]ssh.PublicKey{nlHost: nl.hostSigner.PublicKey()})
	servers := ParseServers("NL|" + nlHost + "|root|env:TG_TEST_SYSLOGNG_PROBE_KEY|/mnt/logs/syslog-ng")
	m := &Module{servers: servers, runner: probeRunner(kh, nl)}

	// Everything a non-empty-values check would look at is populated.
	if len(servers) != 1 {
		t.Fatalf("fixture must parse exactly one complete row, got %+v", servers)
	}
	s := servers[0]
	if s.Site == "" || s.SSHHost == "" || s.SSHUser == "" || s.KeyRef == "" || s.BasePath == "" {
		t.Fatalf("fixture row must be complete: %+v", s)
	}
	if v, err := s.KeyRef.Resolve(); err != nil || strings.TrimSpace(v) == "" {
		t.Fatalf("fixture key ref must resolve to non-empty material (err=%v)", err)
	}
	if _, err := os.Stat(kh); err != nil {
		t.Fatalf("fixture known_hosts must exist: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := m.SelfTest(ctx, "alice@example")
	if err == nil {
		t.Fatalf("a server that refuses the key MUST fail the test — a configured-values check would pass here: %+v", res)
	}
	if !strings.Contains(res.Detail, "authorized_keys") {
		t.Errorf("Detail must name the credential/permission problem specifically, got %q", res.Detail)
	}
	if !strings.Contains(res.Summary, "0 of 1") {
		t.Errorf("Summary must say how many servers answered, got %q", res.Summary)
	}
	nl.assertRanNothing(t)
}

// The host-key classes, which send an operator to two completely different places.
func TestSelfTestClassifiesHostKeyFailures(t *testing.T) {
	t.Run("host key absent from known_hosts", func(t *testing.T) {
		clientSigner, clientPEM := genSigner(t)
		t.Setenv("TG_TEST_SYSLOGNG_PROBE_KEY", string(clientPEM))
		nl := serveProbeServer(t, nlHost, clientSigner.PublicKey())
		// known_hosts pins a DIFFERENT host entirely, so this server is unknown.
		other, _ := genSigner(t)
		kh := writeProbeKnownHosts(t, map[string]ssh.PublicKey{"someotherhost": other.PublicKey()})
		m := &Module{servers: ParseServers("NL|" + nlHost + "|root|env:TG_TEST_SYSLOGNG_PROBE_KEY|/l"), runner: probeRunner(kh, nl)}

		res, err := m.SelfTest(context.Background(), "alice@example")
		if err == nil {
			t.Fatalf("an unverifiable host key must fail the test: %+v", res)
		}
		if !strings.Contains(res.Detail, "not in the known_hosts file") {
			t.Errorf("Detail must say the host key is unknown, got %q", res.Detail)
		}
		nl.assertRanNothing(t)
	})

	t.Run("host key changed from the pinned one", func(t *testing.T) {
		clientSigner, clientPEM := genSigner(t)
		t.Setenv("TG_TEST_SYSLOGNG_PROBE_KEY", string(clientPEM))
		nl := serveProbeServer(t, nlHost, clientSigner.PublicKey())
		pinnedElsewhere, _ := genSigner(t) // known_hosts pins a different key FOR THIS HOST
		kh := writeProbeKnownHosts(t, map[string]ssh.PublicKey{nlHost: pinnedElsewhere.PublicKey()})
		m := &Module{servers: ParseServers("NL|" + nlHost + "|root|env:TG_TEST_SYSLOGNG_PROBE_KEY|/l"), runner: probeRunner(kh, nl)}

		res, err := m.SelfTest(context.Background(), "alice@example")
		if err == nil {
			t.Fatalf("a changed host key must fail the test: %+v", res)
		}
		if !strings.Contains(res.Detail, "does NOT match the one pinned") {
			t.Errorf("Detail must distinguish a CHANGED key from an unknown one, got %q", res.Detail)
		}
		if !strings.Contains(res.Detail, "out of band") {
			t.Errorf("Detail must tell the operator to verify out of band before trusting it, got %q", res.Detail)
		}
		nl.assertRanNothing(t)
	})
}

// The transport class: nothing is listening. This is the other half of the killing oracle — the
// configuration is complete and the module still cannot be shown to work.
func TestSelfTestFailsAgainstAnUnreachableServer(t *testing.T) {
	_, clientPEM := genSigner(t)
	t.Setenv("TG_TEST_SYSLOGNG_PROBE_KEY", string(clientPEM))
	hostSigner, _ := genSigner(t)
	kh := writeProbeKnownHosts(t, map[string]ssh.PublicKey{nlHost: hostSigner.PublicKey()})

	// A REAL closed port: listen, take the address, close the listener. The dial that follows is refused by
	// the kernel, not by a fake returning an error somebody typed.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closedAddr := l.Addr().String()
	_ = l.Close()

	r := &nativeRunner{
		knownHosts:     kh,
		connectTimeout: 2 * time.Second,
		dial: func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", closedAddr)
		},
	}
	m := &Module{servers: ParseServers("NL|" + nlHost + "|root|env:TG_TEST_SYSLOGNG_PROBE_KEY|/l"), runner: r}

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("an unreachable syslog server MUST be an error, not a pass: %+v", res)
	}
	if !strings.Contains(res.Detail, "nothing accepted a connection") {
		t.Errorf("Detail must say the server was unreachable, got %q", res.Detail)
	}
}

// Fail-closed, and the reason it is silent in production: with no known_hosts file every read from every
// server is refused before it is attempted, and nothing anywhere says so.
func TestSelfTestNamesTheMissingKnownHostsKnob(t *testing.T) {
	_, clientPEM := genSigner(t)
	t.Setenv("TG_TEST_SYSLOGNG_PROBE_KEY", string(clientPEM))
	dialed := 0
	r := &nativeRunner{knownHosts: "", connectTimeout: time.Second, dial: countingDial(&dialed)}
	m := &Module{servers: ParseServers("NL|" + nlHost + "|root|env:TG_TEST_SYSLOGNG_PROBE_KEY|/l"), runner: r}

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("no known_hosts must fail the test: %+v", res)
	}
	if !strings.Contains(res.Detail, KnownHostsEnv) {
		t.Errorf("Detail must name the %s knob, got %q", KnownHostsEnv, res.Detail)
	}
	if dialed != 0 {
		t.Errorf("the probe must refuse BEFORE dialing (dials=%d)", dialed)
	}
}

// A key reference that will not resolve, or resolves to something that is not a private key, is a TG-side
// secret fault — and the report must name the REFERENCE and never a byte of material.
func TestSelfTestKeyFaultsAreTGSideAndLeakNothing(t *testing.T) {
	const material = "not-a-private-key-CANARY-9f3a1b"
	cases := []struct {
		name, env, value string
		wantDetail       string
	}{
		{"reference resolves to nothing", "TG_TEST_SYSLOGNG_PROBE_ABSENT", "", "did not resolve"},
		{"reference resolves to garbage", "TG_TEST_SYSLOGNG_PROBE_GARBAGE", material, "did not parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.value != "" {
				t.Setenv(tc.env, tc.value)
			}
			hostSigner, _ := genSigner(t)
			kh := writeProbeKnownHosts(t, map[string]ssh.PublicKey{nlHost: hostSigner.PublicKey()})
			dialed := 0
			r := &nativeRunner{knownHosts: kh, connectTimeout: time.Second, dial: countingDial(&dialed)}
			m := &Module{servers: ParseServers("NL|" + nlHost + "|root|env:" + tc.env + "|/l"), runner: r}

			res, err := m.SelfTest(context.Background(), "alice@example")
			if err == nil {
				t.Fatalf("an unusable key must fail the test: %+v", res)
			}
			if !strings.Contains(res.Detail, tc.wantDetail) || !strings.Contains(res.Detail, "TG-side secret problem") {
				t.Errorf("Detail must name the key fault as a TG-side one, got %q", res.Detail)
			}
			if !strings.Contains(res.Detail, tc.env) {
				t.Errorf("Detail must name the failing REFERENCE so the operator knows which row, got %q", res.Detail)
			}
			for _, s := range []string{res.Summary, res.Detail, err.Error()} {
				if strings.Contains(s, "CANARY") {
					t.Errorf("resolved key material leaked into operator-facing text: %q", s)
				}
			}
			if dialed != 0 {
				t.Errorf("the probe must refuse BEFORE dialing (dials=%d)", dialed)
			}
		})
	}
}

// One bad row costs exactly one site its logs, and the report has to say WHICH — a green that meant "most of
// your sites work" is the silent partial this module already suffers from.
func TestSelfTestPartialFailureNamesTheSiteThatLostItsLogs(t *testing.T) {
	clientSigner, clientPEM := genSigner(t)
	t.Setenv("TG_TEST_SYSLOGNG_PROBE_KEY", string(clientPEM))
	otherSigner, _ := genSigner(t)

	nl := serveProbeServer(t, nlHost, clientSigner.PublicKey()) // healthy
	gr := serveProbeServer(t, grHost, otherSigner.PublicKey())  // refuses TG's key
	kh := writeProbeKnownHosts(t, map[string]ssh.PublicKey{
		nlHost: nl.hostSigner.PublicKey(),
		grHost: gr.hostSigner.PublicKey(),
	})
	m := &Module{servers: probeServers(), runner: probeRunner(kh, nl, gr)}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := m.SelfTest(ctx, "alice@example")
	if err == nil {
		t.Fatalf("one refused server must fail the whole test: %+v", res)
	}
	if !strings.Contains(res.Summary, "1 of 2") {
		t.Errorf("Summary must count the servers that answered, got %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "root@"+nlHost+" [NL] ok") || !strings.Contains(res.Summary, "svc@"+grHost+" [GR] FAILED") {
		t.Errorf("Summary must say which site failed and which did not, got %q", res.Summary)
	}
	if !strings.Contains(res.Detail, grHost) {
		t.Errorf("Detail must attribute the fault to the failing server, got %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "site with NO device logs during triage") {
		t.Errorf("Detail must say what the failure costs, got %q", res.Detail)
	}
	nl.assertRanNothing(t)
	gr.assertRanNothing(t)
}

// An empty (or entirely mis-typed) server list is the state ParseServers produces silently, and it is worth
// its own honest answer rather than a pass over zero servers.
func TestSelfTestNoServersIsAnHonestFailure(t *testing.T) {
	m := NewModule(ParseServers("broken|only-two-fields"), &fakeRunner{})
	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("no configured servers must fail the test: %+v", res)
	}
	if !strings.Contains(res.Detail, "SKIPPED as malformed") {
		t.Errorf("Detail must explain that a malformed row is dropped silently, got %q", res.Detail)
	}
}

// A transport that cannot open a session without running a command gets an honest "no probe" rather than a
// fabricated pass — and must not fall back to executing something.
func TestSelfTestRefusesToProbeANonSessionTransport(t *testing.T) {
	f := &fakeRunner{}
	m := NewModule(testServers(), f)
	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("a transport with no session capability must not report a pass: %+v", res)
	}
	if f.calls != 0 {
		t.Errorf("the probe must not run a command as a fallback (calls=%d, argv=%v)", f.calls, f.gotArgv)
	}
}

// The console holds an operator on a spinner and moduletest allows one attempt: a stalled server must lose to
// the deadline rather than hang.
func TestSelfTestRespectsTheDeadlineAgainstAStalledServer(t *testing.T) {
	_, clientPEM := genSigner(t)
	t.Setenv("TG_TEST_SYSLOGNG_PROBE_KEY", string(clientPEM))
	hostSigner, _ := genSigner(t)
	kh := writeProbeKnownHosts(t, map[string]ssh.PublicKey{nlHost: hostSigner.PublicKey()})

	clientEnd, serverEnd := net.Pipe() // the far end never speaks SSH
	defer func() { _ = serverEnd.Close() }()
	r := &nativeRunner{
		knownHosts:     kh,
		connectTimeout: 30 * time.Second, // deliberately long: the ctx must win, not this
		dial:           func(context.Context, string) (net.Conn, error) { return clientEnd, nil },
	}
	m := &Module{servers: ParseServers("NL|" + nlHost + "|root|env:TG_TEST_SYSLOGNG_PROBE_KEY|/l"), runner: r}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := m.SelfTest(ctx, "alice@example")
	if err == nil {
		t.Fatalf("a stalled server must fail once the deadline passes: %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the deadline was not enforced: SelfTest blocked %v", elapsed)
	}
	if !strings.Contains(res.Detail, "time budget") {
		t.Errorf("Detail must attribute the failure to the time budget, got %q", res.Detail)
	}
}

// NewModule must default to the production native runner AND hand the SAME runner to the tools: a probe over
// one transport certifying reads that travel over another would prove nothing at all.
func TestNewModuleDefaultsToNativeRunnerAndSharesItWithTools(t *testing.T) {
	m := NewModule(testServers(), nil)
	if _, ok := m.runner.(*nativeRunner); !ok {
		t.Fatalf("a nil runner must default to the native in-process SSH runner, got %T", m.runner)
	}
	if _, ok := m.runner.(sessionOpener); !ok {
		t.Fatal("the production runner must be able to open a session without running a command")
	}
	tl := findTool(m.Tools(), "get-host-logs")
	if tl == nil {
		t.Fatal("Tools() must return the read-only syslog-ng tools")
	}
	if got := tl.(getHostLogsTool).b.runner; got != m.runner {
		t.Errorf("the tools must use the SAME runner the probe exercises, got %p want %p", got, m.runner)
	}
	if len(m.Servers()) != len(testServers()) {
		t.Errorf("Servers() must report the configured rows")
	}
}
