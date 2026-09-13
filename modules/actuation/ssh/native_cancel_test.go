package ssh

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
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/territory-grounder/grounder/core/config"
)

// TG-80 P1-4 oracles: a cancelled actuation must SIGNAL the remote command dead over the SSH channel
// before the transport closes — TERM, then KILL if TERM is ignored — and surface as the typed
// ErrRemoteCancelled wrapping the context error. Before this the runner closed the TCP link on ctx.Done
// and the remote process ran on, orphaned on the target; against the fixture below that shape shows as
// ZERO signal requests ever reaching the server, which is exactly what the killing mutation (revert to
// sess.Run + a transport-closing watchdog) reproduces.
//
// The in-process sshd is a real x/crypto server on a loopback pair (ported from the syslog-ng read
// lane's fixture); it runs a "command" that only ends when signalled.

func tg80GenSigner(t *testing.T) (cryptossh.Signer, []byte) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := cryptossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	signer, err := cryptossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return signer, pem.EncodeToMemory(block)
}

func tg80LoopbackPipe(t *testing.T) (client, server net.Conn) {
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

// tg80ServeSignalSSH serves one session whose exec'd "command" never exits on its own: it ends only on a
// signal request — TERM (unless ignoreTERM) or KILL — replying exit-signal and closing the channel. Every
// signal name received is sent on signals, in order.
func tg80ServeSignalSSH(t *testing.T, conn net.Conn, hostSigner cryptossh.Signer, wantClientPub cryptossh.PublicKey, ignoreTERM bool, signals chan<- string) {
	t.Helper()
	cfg := &cryptossh.ServerConfig{
		PublicKeyCallback: func(_ cryptossh.ConnMetadata, key cryptossh.PublicKey) (*cryptossh.Permissions, error) {
			if !bytes.Equal(key.Marshal(), wantClientPub.Marshal()) {
				return nil, fmt.Errorf("unknown client key")
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
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(cryptossh.UnknownChannelType, "only session is served")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}
		for req := range chReqs {
			switch req.Type {
			case "exec":
				_ = req.Reply(true, nil) // the "command" is now running and will not exit on its own
			case "signal":
				var p struct{ Signal string }
				_ = cryptossh.Unmarshal(req.Payload, &p)
				signals <- p.Signal
				if p.Signal == "KILL" || (p.Signal == "TERM" && !ignoreTERM) {
					_, _ = ch.SendRequest("exit-signal", false, cryptossh.Marshal(struct {
						Signal     string
						CoreDumped bool
						Error      string
						Lang       string
					}{Signal: p.Signal}))
					_ = ch.Close()
					return
				}
			default:
				_ = req.Reply(false, nil)
			}
		}
		return
	}
}

func tg80Runner(t *testing.T, ignoreTERM bool) (*nativeRunner, []string, <-chan string) {
	t.Helper()
	hostSigner, _ := tg80GenSigner(t)
	clientSigner, clientPEM := tg80GenSigner(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "actuator")
	if err := os.WriteFile(keyPath, clientPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	khPath := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(khPath, []byte(knownhosts.Line([]string{"web01"}, hostSigner.PublicKey())+"\n"), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	signals := make(chan string, 4)
	r := &nativeRunner{
		knownHosts:     khPath,
		keyRef:         config.SecretRef("file:" + keyPath),
		connectTimeout: defaultConnectTimeout,
		dial: func(context.Context, string) (net.Conn, error) {
			c, s := tg80LoopbackPipe(t)
			go tg80ServeSignalSSH(t, s, hostSigner, clientSigner.PublicKey(), ignoreTERM, signals)
			return c, nil
		},
	}
	m := New("web01", "svc", &fakeRunner{})
	return r, m.sshArgv([]string{"sleep", "3600"}), signals
}

// A deadline during a running command sends TERM over the channel and returns the typed cancellation.
func TestTG80CancelSignalsTERMBeforeClosingTransport(t *testing.T) {
	old := killGrace
	killGrace = 200 * time.Millisecond
	t.Cleanup(func() { killGrace = old })
	r, argv, signals := tg80Runner(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := r.Run(ctx, argv, nil)
	if !errors.Is(err, ErrRemoteCancelled) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want ErrRemoteCancelled wrapping the deadline, got %v", err)
	}
	select {
	case s := <-signals:
		if s != "TERM" {
			t.Fatalf("first signal = %q, want TERM", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the server never received a signal — the transport was closed under the running command (the orphaned-process defect)")
	}
}

// A command that ignores TERM is escalated to KILL within the grace window.
func TestTG80CancelEscalatesToKILLWhenTERMIgnored(t *testing.T) {
	old := killGrace
	killGrace = 100 * time.Millisecond
	t.Cleanup(func() { killGrace = old })
	r, argv, signals := tg80Runner(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Run(ctx, argv, nil)
	if !errors.Is(err, ErrRemoteCancelled) {
		t.Fatalf("want ErrRemoteCancelled, got %v", err)
	}
	var got []string
	for len(got) < 2 {
		select {
		case s := <-signals:
			got = append(got, s)
		case <-time.After(2 * time.Second):
			t.Fatalf("signals seen %v — want TERM then KILL (TERM-ignoring command must be escalated)", got)
		}
	}
	if got[0] != "TERM" || got[1] != "KILL" {
		t.Fatalf("signal order %v, want [TERM KILL]", got)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("escalation took %s — the grace windows are not bounding the kill", el)
	}
}

// The happy path is byte-for-byte the old behavior: a command that exits on its own returns its result.
func TestTG80UncancelledRunStillReturnsExitStatus(t *testing.T) {
	r, argv, _ := tg80Runner(t, false)
	// Re-point dial at a server whose exec exits immediately (the syslog-ng fixture shape).
	hostSigner, _ := tg80GenSigner(t)
	clientSigner, clientPEM := tg80GenSigner(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "k")
	_ = os.WriteFile(keyPath, clientPEM, 0o600)
	khPath := filepath.Join(dir, "kh")
	_ = os.WriteFile(khPath, []byte(knownhosts.Line([]string{"web01"}, hostSigner.PublicKey())+"\n"), 0o600)
	r.keyRef, r.knownHosts = config.SecretRef("file:"+keyPath), khPath
	r.dial = func(context.Context, string) (net.Conn, error) {
		c, s := tg80LoopbackPipe(t)
		go func() {
			cfg := &cryptossh.ServerConfig{PublicKeyCallback: func(_ cryptossh.ConnMetadata, key cryptossh.PublicKey) (*cryptossh.Permissions, error) {
				if !bytes.Equal(key.Marshal(), clientSigner.PublicKey().Marshal()) {
					return nil, fmt.Errorf("unknown key")
				}
				return nil, nil
			}}
			cfg.AddHostKey(hostSigner)
			sc, chans, reqs, err := cryptossh.NewServerConn(s, cfg)
			if err != nil {
				return
			}
			defer func() { _ = sc.Close() }()
			go cryptossh.DiscardRequests(reqs)
			for newCh := range chans {
				ch, chReqs, err := newCh.Accept()
				if err != nil {
					return
				}
				for req := range chReqs {
					if req.Type == "exec" {
						_ = req.Reply(true, nil)
						_, _ = ch.Write([]byte("done\n"))
						_, _ = ch.SendRequest("exit-status", false, cryptossh.Marshal(struct{ Status uint32 }{Status: 3}))
						_ = ch.Close()
						return
					}
					_ = req.Reply(false, nil)
				}
			}
		}()
		return c, nil
	}
	res, err := r.Run(context.Background(), argv, nil)
	if err != nil {
		t.Fatalf("uncancelled run: %v", err)
	}
	if res.ExitCode != 3 || string(res.Stdout) != "done\n" {
		t.Fatalf("result = exit %d stdout %q, want exit 3 / done", res.ExitCode, res.Stdout)
	}
}
