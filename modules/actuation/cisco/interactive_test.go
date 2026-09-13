package cisco

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/territory-grounder/grounder/core/config"
)

// --- in-process SSH test harness (ported from modules/observability/syslogng/native_test.go) ---

func genSigner(t *testing.T) (cryptossh.Signer, []byte) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := cryptossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := cryptossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer, pem.EncodeToMemory(block)
}

func writeKnownHosts(t *testing.T, host string, pub cryptossh.PublicKey) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(p, []byte(knownhosts.Line([]string{host}, pub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func loopbackPipe(t *testing.T) (client, server net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	type accepted struct {
		c   net.Conn
		err error
	}
	ch := make(chan accepted, 1)
	go func() { c, err := l.Accept(); ch <- accepted{c, err} }()
	client, err = net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	a := <-ch
	if a.err != nil {
		t.Fatal(a.err)
	}
	t.Cleanup(func() { _ = client.Close(); _ = a.c.Close() })
	return client, a.c
}

// serveFakeCisco serves ONE SSH connection acting as an IOS/ASA device: it requires wantClientPub, accepts a
// session, honors pty-req + shell, then drives a prompt-based CLI. It records the commands it received
// (EXCLUDING the runner's own pager-off) and answers a `show` with canned output. The prompt is
// "ciscoasa# " so it matches defaultPromptRE. A refused handshake just returns (the host-key oracle).
func serveFakeCisco(t *testing.T, conn net.Conn, hostSigner cryptossh.Signer, wantClientPub cryptossh.PublicKey, showOut string, gotCmds chan<- string) {
	t.Helper()
	cfg := &cryptossh.ServerConfig{
		PublicKeyCallback: func(_ cryptossh.ConnMetadata, key cryptossh.PublicKey) (*cryptossh.Permissions, error) {
			if !bytes.Equal(key.Marshal(), wantClientPub.Marshal()) {
				return nil, errors.New("unknown client key")
			}
			return nil, nil
		},
	}
	cfg.AddHostKey(hostSigner)
	sc, chans, reqs, err := cryptossh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sc.Close() }()
	go cryptossh.DiscardRequests(reqs)
	const prompt = "ciscoasa# "
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(cryptossh.UnknownChannelType, "only session")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}
		// Handle channel requests (pty-req, shell) in a goroutine; then drive the CLI over ch.
		go func() {
			for req := range chReqs {
				switch req.Type {
				case "pty-req", "env":
					_ = req.Reply(true, nil)
				case "shell":
					_ = req.Reply(true, nil)
					_, _ = ch.Write([]byte(prompt)) // initial prompt
				default:
					_ = req.Reply(false, nil)
				}
			}
		}()
		// Read client input line by line and answer.
		var line bytes.Buffer
		b := make([]byte, 256)
		for {
			n, rerr := ch.Read(b)
			if n > 0 {
				line.Write(b[:n])
				for {
					s := line.String()
					idx := strings.IndexAny(s, "\r\n")
					if idx < 0 {
						break
					}
					cmd := strings.TrimSpace(s[:idx])
					// advance past the line terminator(s)
					rest := s[idx:]
					rest = strings.TrimLeft(rest, "\r\n")
					line.Reset()
					line.WriteString(rest)
					if cmd == "" {
						continue
					}
					// Echo the command (real devices do), then respond.
					_, _ = ch.Write([]byte(cmd + "\r\n"))
					switch {
					case cmd == "exit":
						_ = ch.Close()
						return
					case strings.HasPrefix(cmd, "terminal "):
						// pager-off: no output, just the prompt.
						_, _ = ch.Write([]byte(prompt))
					default:
						gotCmds <- cmd
						_, _ = ch.Write([]byte(showOut + "\r\n" + prompt))
					}
				}
			}
			if rerr != nil {
				return
			}
		}
	}
}

// ciscoTestRunner builds an interactiveRunner whose dial returns the client end of an in-process pipe served
// by serveFakeCisco. Returns the runner and a channel of commands the fake received.
func ciscoTestRunner(t *testing.T, showOut string, legacy bool) (*interactiveRunner, <-chan string) {
	t.Helper()
	hostSigner, _ := genSigner(t)
	clientSigner, clientPEM := genSigner(t)
	t.Setenv("CISCO_TEST_KEY", string(clientPEM))
	kh := writeKnownHosts(t, "cisco-dev:22", hostSigner.PublicKey())

	gotCmds := make(chan string, 4)
	dev := Device{
		Host: "cisco-dev", Identity: "netops", KeyRef: config.SecretRef("env:CISCO_TEST_KEY"),
		KnownHosts: kh, LegacyCrypto: legacy,
	}
	r := NewInteractiveRunner(dev)
	r.connectTimeout = 3 * time.Second
	r.ioTimeout = 3 * time.Second
	r.dial = func(_ context.Context, _ string) (net.Conn, error) {
		client, server := loopbackPipe(t)
		go serveFakeCisco(t, server, hostSigner, clientSigner.PublicKey(), showOut, gotCmds)
		return client, nil
	}
	return r, gotCmds
}

// The end-to-end read: dial → host-key verify → pty+shell → pager-off → one show → captured output, with the
// echo and the trailing prompt stripped. KILLING MUTATION: drop the pager-off send in RunShow → the fake never
// records "terminal length 0" but the captured output would still round-trip; instead assert the runner sent
// pager-off before the command (below).
func TestRunShowEndToEndAgainstFakeCisco(t *testing.T) {
	r, gotCmds := ciscoTestRunner(t, "access-list OUTSIDE line 1 permit tcp any any eq 443", false)
	res, err := r.RunShow(context.Background(), "show access-list")
	if err != nil {
		t.Fatalf("RunShow: %v", err)
	}
	out := string(res.Stdout)
	if !strings.Contains(out, "access-list OUTSIDE line 1 permit tcp any any eq 443") {
		t.Errorf("output missing the show payload: %q", out)
	}
	// The echo of the command and the trailing prompt must be stripped.
	if strings.Contains(out, "show access-list") {
		t.Errorf("the echoed command leaked into the output: %q", out)
	}
	if strings.Contains(out, "ciscoasa#") {
		t.Errorf("the device prompt leaked into the output: %q", out)
	}
	// The runner must have sent the show command (the fake records non-pager commands).
	select {
	case c := <-gotCmds:
		if c != "show access-list" {
			t.Errorf("device received %q, want the show command", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the device never received the show command")
	}
}

// Legacy crypto arms the deprecated KEX/ciphers without breaking a modern handshake (the fake uses modern
// algorithms, so this proves the appended legacy set does not displace the secure defaults).
func TestRunShowWithLegacyCryptoStillHandshakes(t *testing.T) {
	r, _ := ciscoTestRunner(t, "Cisco Adaptive Security Appliance Version 9.8", true)
	res, err := r.RunShow(context.Background(), "show version")
	if err != nil {
		t.Fatalf("legacy-crypto RunShow: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "Version 9.8") {
		t.Errorf("output = %q", res.Stdout)
	}
}

// cleanOutput must strip the RUNNER'S configured prompt, not the package default — else a wiring slice that
// pins a device-specific prompt leaks it into the read output. KILLING MUTATION: revert cleanOutput to
// defaultPromptRE → the pinned "myrtr(config)>" prompt survives and this reddens.
func TestCleanOutputStripsThePinnedPrompt(t *testing.T) {
	pinned := regexp.MustCompile(`myrtr[#>] ?$`)
	captured := "show version\r\nCisco IOS Software, Version 15.2\r\nmyrtr# "
	out := string(cleanOutput(captured, "show version", pinned))
	if strings.Contains(out, "myrtr#") {
		t.Errorf("the pinned prompt leaked into the output: %q", out)
	}
	if !strings.Contains(out, "Version 15.2") {
		t.Errorf("the payload was lost: %q", out)
	}
	if strings.Contains(out, "show version") {
		t.Errorf("the echoed command leaked: %q", out)
	}
}

// --- fail-closed oracles ---

func TestRunShowFailsClosedWithoutKnownHosts(t *testing.T) {
	dials := 0
	r := NewInteractiveRunner(Device{Host: "x", Identity: "u", KeyRef: config.SecretRef("env:UNUSED")})
	r.dial = func(context.Context, string) (net.Conn, error) { dials++; return nil, errors.New("must not dial") }
	_, err := r.RunShow(context.Background(), "show version")
	if err == nil || !strings.Contains(err.Error(), KnownHostsEnv) {
		t.Fatalf("no known_hosts must refuse and name %s, got %v", KnownHostsEnv, err)
	}
	if dials != 0 {
		t.Errorf("must refuse BEFORE dialing, dials=%d", dials)
	}
}

func TestRunShowFailsClosedOnUnresolvableCredential(t *testing.T) {
	kh := writeKnownHosts(t, "x:22", func() cryptossh.PublicKey { s, _ := genSigner(t); return s.PublicKey() }())
	dials := 0
	r := NewInteractiveRunner(Device{Host: "x", Identity: "u", KeyRef: config.SecretRef("env:CISCO_ABSENT_KEY"), KnownHosts: kh})
	r.dial = func(context.Context, string) (net.Conn, error) { dials++; return nil, errors.New("must not dial") }
	if _, err := r.RunShow(context.Background(), "show version"); err == nil {
		t.Fatal("an unresolvable credential must fail closed")
	}
	if dials != 0 {
		t.Errorf("must refuse BEFORE dialing, dials=%d", dials)
	}
}

func TestRunShowRefusesUnknownHostKey(t *testing.T) {
	// known_hosts pins a DIFFERENT host key than the fake presents ⇒ the handshake must be refused.
	otherSigner, _ := genSigner(t)
	kh := writeKnownHosts(t, "cisco-dev:22", otherSigner.PublicKey())
	hostSigner, _ := genSigner(t)
	clientSigner, clientPEM := genSigner(t)
	t.Setenv("CISCO_TEST_KEY", string(clientPEM))
	r := NewInteractiveRunner(Device{Host: "cisco-dev", Identity: "u", KeyRef: config.SecretRef("env:CISCO_TEST_KEY"), KnownHosts: kh})
	r.connectTimeout = 3 * time.Second
	r.dial = func(_ context.Context, _ string) (net.Conn, error) {
		client, server := loopbackPipe(t)
		go serveFakeCisco(t, server, hostSigner, clientSigner.PublicKey(), "x", make(chan string, 1))
		return client, nil
	}
	if _, err := r.RunShow(context.Background(), "show version"); err == nil {
		t.Fatal("a host key not in known_hosts must be refused (impersonation guard)")
	}
}

func TestRunShowCtxDeadlineAborts(t *testing.T) {
	hostSigner, _ := genSigner(t)
	clientSigner, clientPEM := genSigner(t)
	t.Setenv("CISCO_TEST_KEY", string(clientPEM))
	kh := writeKnownHosts(t, "cisco-dev:22", hostSigner.PublicKey())
	r := NewInteractiveRunner(Device{Host: "cisco-dev", Identity: "u", KeyRef: config.SecretRef("env:CISCO_TEST_KEY"), KnownHosts: kh})
	r.connectTimeout = 3 * time.Second
	r.ioTimeout = 3 * time.Second
	r.dial = func(_ context.Context, _ string) (net.Conn, error) {
		client, server := loopbackPipe(t)
		// A server that authenticates but NEVER sends a prompt — the expect must abort on the ctx deadline.
		go func() {
			cfg := &cryptossh.ServerConfig{PublicKeyCallback: func(_ cryptossh.ConnMetadata, key cryptossh.PublicKey) (*cryptossh.Permissions, error) {
				if !bytes.Equal(key.Marshal(), clientSigner.PublicKey().Marshal()) {
					return nil, errors.New("no")
				}
				return nil, nil
			}}
			cfg.AddHostKey(hostSigner)
			scc, chs, rqs, err := cryptossh.NewServerConn(server, cfg)
			if err != nil {
				return
			}
			defer func() { _ = scc.Close() }()
			go cryptossh.DiscardRequests(rqs)
			for nc := range chs {
				c, cr, _ := nc.Accept()
				go func() {
					for req := range cr {
						_ = req.Reply(true, nil)
					}
				}()
				_ = c // never write a prompt
			}
		}()
		return client, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, err := r.RunShow(ctx, "show version"); err == nil {
		t.Fatal("a device that never presents a prompt must abort on the deadline, not hang")
	}
}
