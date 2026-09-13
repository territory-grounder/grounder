package syslogng

// Oracles for the production NATIVE SSH runner (no subprocess — the worker's distroless image has no
// ssh binary and no shell). Everything here is pure Go and runs in CI with no network: the end-to-end
// proof serves a REAL x/crypto SSH server over a net.Pipe, so the client-side handshake, the
// known_hosts host-key verification, the in-memory key auth, the exec payload, and the exit-status
// plumbing are all exercised for real. The fail-closed oracles prove no read can happen without a
// verifiable host key and that a key failure names the REFERENCE only, never material.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/territory-grounder/grounder/core/config"
)

// genSigner mints a fresh ed25519 keypair, returning the ssh.Signer and its OpenSSH PEM encoding.
func genSigner(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return signer, pem.EncodeToMemory(block)
}

// writeKnownHosts writes a one-entry OpenSSH known_hosts file pinning host → pub.
func writeKnownHosts(t *testing.T, host string, pub ssh.PublicKey) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(p, []byte(knownhosts.Line([]string{host}, pub)+"\n"), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	return p
}

// loopbackPipe returns an in-process, kernel-buffered conn pair (loopback TCP, the same construction
// x/crypto's own handshake tests use — a raw net.Pipe deadlocks the version exchange because both
// sides block in their first unbuffered Write). No packet leaves the host; CI needs no network.
func loopbackPipe(t *testing.T) (client, server net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("loopback listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	type accepted struct {
		c   net.Conn
		err error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, err := l.Accept()
		ch <- accepted{c, err}
	}()
	client, err = net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("loopback dial: %v", err)
	}
	a := <-ch
	if a.err != nil {
		t.Fatalf("loopback accept: %v", a.err)
	}
	t.Cleanup(func() { _ = client.Close(); _ = a.c.Close() })
	return client, a.c
}

// serveOneSSH serves ONE real x/crypto SSH server connection on conn: it requires the client to
// authenticate with wantClientPub, accepts one session channel, records the exec command it receives,
// writes stdout, returns exit-status 0, and closes. A refused handshake (the host-key rejection
// oracle) simply returns.
func serveOneSSH(t *testing.T, conn net.Conn, hostSigner ssh.Signer, wantClientPub ssh.PublicKey, stdout string, gotCmd chan<- string) {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if !bytes.Equal(key.Marshal(), wantClientPub.Marshal()) {
				return nil, fmt.Errorf("unknown client key")
			}
			return nil, nil
		},
	}
	cfg.AddHostKey(hostSigner)
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return // the client refused us (host-key rejection) or the transport died
	}
	defer func() { _ = sc.Close() }()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only session is served")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}
		for req := range chReqs {
			if req.Type != "exec" {
				_ = req.Reply(false, nil)
				continue
			}
			var p struct{ Command string }
			_ = ssh.Unmarshal(req.Payload, &p)
			_ = req.Reply(true, nil)
			gotCmd <- p.Command
			_, _ = ch.Write([]byte(stdout))
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: 0}))
			_ = ch.Close()
			return
		}
	}
}

// countingDial fails the run if the runner ever dials — the fail-closed oracles must refuse BEFORE
// any connection attempt.
func countingDial(calls *int) func(context.Context, string) (net.Conn, error) {
	return func(context.Context, string) (net.Conn, error) {
		*calls++
		return nil, errors.New("dial must not be reached")
	}
}

func nlServer(keyRef string) Server {
	return Server{Site: "NL", SSHHost: "dc1syslogng01", SSHUser: "root", KeyRef: config.SecretRef(keyRef), BasePath: DefaultBasePath, HostPrefix: "dc1"}
}

var tailArgv = []string{"tail", "-n", "200", "--", "/mnt/logs/syslog-ng/dc1fw01/today.log"}

// ---- fail-closed: host-key verification is mandatory ----

func TestNativeRunnerFailsClosedWithoutKnownHosts(t *testing.T) {
	calls := 0
	r := &nativeRunner{knownHosts: "", connectTimeout: time.Second, dial: countingDial(&calls)}
	_, err := r.Run(context.Background(), nlServer("env:SYSLOGNG_TEST_KEY_UNUSED"), tailArgv)
	if err == nil {
		t.Fatal("no known_hosts configured must refuse the read")
	}
	if !strings.Contains(err.Error(), KnownHostsEnv) {
		t.Errorf("the refusal must name the %s knob, got %q", KnownHostsEnv, err)
	}
	if calls != 0 {
		t.Errorf("must refuse BEFORE dialing (dials=%d)", calls)
	}
}

func TestNativeRunnerFailsClosedOnMissingKnownHostsFile(t *testing.T) {
	calls := 0
	r := &nativeRunner{knownHosts: filepath.Join(t.TempDir(), "absent"), connectTimeout: time.Second, dial: countingDial(&calls)}
	_, err := r.Run(context.Background(), nlServer("env:SYSLOGNG_TEST_KEY_UNUSED"), tailArgv)
	if err == nil {
		t.Fatal("a missing known_hosts file must refuse the read")
	}
	if !strings.Contains(err.Error(), "known_hosts") {
		t.Errorf("the refusal must say known_hosts is unusable, got %q", err)
	}
	if calls != 0 {
		t.Errorf("must refuse BEFORE dialing (dials=%d)", calls)
	}
}

// ---- fail-closed: key handling names the REF only, never material ----

func TestNativeRunnerKeyRefUnresolvedNamesRefOnly(t *testing.T) {
	hostSigner, _ := genSigner(t)
	calls := 0
	r := &nativeRunner{knownHosts: writeKnownHosts(t, "dc1syslogng01:22", hostSigner.PublicKey()), connectTimeout: time.Second, dial: countingDial(&calls)}
	_, err := r.Run(context.Background(), nlServer("env:SYSLOGNG_TEST_KEY_ABSENT"), tailArgv)
	if err == nil {
		t.Fatal("an unresolved key ref must refuse the read")
	}
	if !strings.Contains(err.Error(), `"env:SYSLOGNG_TEST_KEY_ABSENT"`) || !strings.Contains(err.Error(), "did not resolve") {
		t.Errorf("the refusal must name the ref, got %q", err)
	}
	if calls != 0 {
		t.Errorf("must refuse BEFORE dialing (dials=%d)", calls)
	}
}

func TestNativeRunnerKeyRefEmptyFailsClosed(t *testing.T) {
	t.Setenv("SYSLOGNG_TEST_KEY_EMPTY", "")
	hostSigner, _ := genSigner(t)
	calls := 0
	r := &nativeRunner{knownHosts: writeKnownHosts(t, "dc1syslogng01:22", hostSigner.PublicKey()), connectTimeout: time.Second, dial: countingDial(&calls)}
	_, err := r.Run(context.Background(), nlServer("env:SYSLOGNG_TEST_KEY_EMPTY"), tailArgv)
	if err == nil {
		t.Fatal("an empty-resolving key ref must refuse the read")
	}
	if !strings.Contains(err.Error(), `"env:SYSLOGNG_TEST_KEY_EMPTY"`) || !strings.Contains(err.Error(), "resolved empty") {
		t.Errorf("the refusal must name the ref and say it resolved empty, got %q", err)
	}
	if calls != 0 {
		t.Errorf("must refuse BEFORE dialing (dials=%d)", calls)
	}
}

func TestNativeRunnerKeyParseFailureNamesRefNeverMaterial(t *testing.T) {
	const material = "not-a-private-key-CANARY-9f3a1b"
	t.Setenv("SYSLOGNG_TEST_KEY_GARBAGE", material)
	hostSigner, _ := genSigner(t)
	calls := 0
	r := &nativeRunner{knownHosts: writeKnownHosts(t, "dc1syslogng01:22", hostSigner.PublicKey()), connectTimeout: time.Second, dial: countingDial(&calls)}
	_, err := r.Run(context.Background(), nlServer("env:SYSLOGNG_TEST_KEY_GARBAGE"), tailArgv)
	if err == nil {
		t.Fatal("an unparseable key must refuse the read")
	}
	if !strings.Contains(err.Error(), `"env:SYSLOGNG_TEST_KEY_GARBAGE"`) || !strings.Contains(err.Error(), "did not parse") {
		t.Errorf("the refusal must name the ref, got %q", err)
	}
	if strings.Contains(err.Error(), "CANARY") {
		t.Errorf("the refusal leaked resolved key material: %q", err)
	}
	if calls != 0 {
		t.Errorf("must refuse BEFORE dialing (dials=%d)", calls)
	}
}

// ---- end to end: a REAL x/crypto SSH server over a net.Pipe ----

func TestNativeRunnerEndToEndAgainstInProcessServer(t *testing.T) {
	hostSigner, _ := genSigner(t)
	clientSigner, clientPEM := genSigner(t)
	t.Setenv("SYSLOGNG_TEST_KEY", string(clientPEM))
	kh := writeKnownHosts(t, "dc1syslogng01:22", hostSigner.PublicKey())

	clientEnd, serverEnd := loopbackPipe(t)
	gotCmd := make(chan string, 1)
	const canned = "Jul 15 12:00:02 dc1fw01 %ASA-6-302014: Teardown TCP connection 1 for outside:192.0.2.9/443\n"
	go serveOneSSH(t, serverEnd, hostSigner, clientSigner.PublicKey(), canned, gotCmd)

	r := &nativeRunner{
		knownHosts:     kh,
		connectTimeout: 5 * time.Second,
		dial: func(_ context.Context, addr string) (net.Conn, error) {
			if addr != "dc1syslogng01:22" {
				t.Errorf("dial addr = %q, want dc1syslogng01:22", addr)
			}
			return clientEnd, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := r.Run(ctx, nlServer("env:SYSLOGNG_TEST_KEY"), tailArgv)
	if err != nil {
		t.Fatalf("end-to-end run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	if string(res.Stdout) != canned {
		t.Errorf("stdout = %q, want the canned server line", res.Stdout)
	}
	select {
	case cmd := <-gotCmd:
		want := `'tail' '-n' '200' '--' '/mnt/logs/syslog-ng/dc1fw01/today.log'`
		if cmd != want {
			t.Errorf("server received exec %q, want the POSIX-quoted fixed argv %q", cmd, want)
		}
	default:
		t.Fatal("the in-process server never received an exec request")
	}
}

func TestNativeRunnerRefusesUnknownHostKey(t *testing.T) {
	hostSigner, _ := genSigner(t)
	pinnedElsewhere, _ := genSigner(t) // known_hosts pins a DIFFERENT key than the server presents
	clientSigner, clientPEM := genSigner(t)
	t.Setenv("SYSLOGNG_TEST_KEY", string(clientPEM))
	kh := writeKnownHosts(t, "dc1syslogng01:22", pinnedElsewhere.PublicKey())

	clientEnd, serverEnd := loopbackPipe(t)
	gotCmd := make(chan string, 1)
	go serveOneSSH(t, serverEnd, hostSigner, clientSigner.PublicKey(), "must never be read\n", gotCmd)

	r := &nativeRunner{
		knownHosts:     kh,
		connectTimeout: 5 * time.Second,
		dial:           func(context.Context, string) (net.Conn, error) { return clientEnd, nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := r.Run(ctx, nlServer("env:SYSLOGNG_TEST_KEY"), tailArgv)
	if err == nil {
		t.Fatal("a host key not matching known_hosts MUST refuse the connection")
	}
	if !strings.Contains(err.Error(), "handshake") {
		t.Errorf("the refusal must surface as a refused handshake, got %q", err)
	}
	select {
	case cmd := <-gotCmd:
		t.Fatalf("no exec may run on an unverified host, but the server received %q", cmd)
	default:
	}
}

func TestNativeRunnerCtxDeadlineAbortsStalledTransport(t *testing.T) {
	hostSigner, _ := genSigner(t)
	_, clientPEM := genSigner(t)
	t.Setenv("SYSLOGNG_TEST_KEY", string(clientPEM))
	kh := writeKnownHosts(t, "dc1syslogng01:22", hostSigner.PublicKey())

	clientEnd, serverEnd := net.Pipe()
	defer func() { _ = serverEnd.Close() }()
	// The far end NEVER speaks SSH: the handshake stalls on the raw pipe until the watchdog closes it.
	r := &nativeRunner{
		knownHosts:     kh,
		connectTimeout: 30 * time.Second, // deliberately long: ctx must win, not this
		dial:           func(context.Context, string) (net.Conn, error) { return clientEnd, nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Run(ctx, nlServer("env:SYSLOGNG_TEST_KEY"), tailArgv)
	if err == nil {
		t.Fatal("a stalled transport must fail once the ctx deadline passes")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("ctx deadline not enforced: run blocked %v", elapsed)
	}
	if !strings.Contains(err.Error(), "deadline") {
		t.Errorf("the failure must attribute the abort to the deadline, got %q", err)
	}
}

// ---- the production default + the quoting contract ----

func TestNewToolsNilRunnerDefaultsToNativeRunner(t *testing.T) {
	tl := findTool(NewTools(testServers(), nil), "get-host-logs").(getHostLogsTool)
	if _, ok := tl.b.runner.(*nativeRunner); !ok {
		t.Fatalf("a nil runner must default to the native in-process SSH runner, got %T", tl.b.runner)
	}
}

func TestRemoteCommandQuotesEachElement(t *testing.T) {
	got := remoteCommand([]string{"grep", "-F", "-m", "500", "--", "it's a match", "/p/f.log"})
	want := `'grep' '-F' '-m' '500' '--' 'it'\''s a match' '/p/f.log'`
	if got != want {
		t.Errorf("remoteCommand = %q, want %q", got, want)
	}
}
