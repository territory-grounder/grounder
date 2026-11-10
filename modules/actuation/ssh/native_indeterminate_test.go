package ssh

// TG-663 (G4A F4): the native runner classifies its failures by WHERE they happen. Everything up to and
// including sess.Start is pre-send (a plain error — nothing was launched); a Wait that fails without an exit
// status after a successful Start is AFTER-SEND and marked actuation.ErrIndeterminate, as is a cancellation of a
// running command (which keeps its ErrRemoteCancelled / context classification too).
//
// KILLING MUTATION (executed 2026-11-07): drop the actuation.MarkIndeterminate wrap on the post-Start Wait error
// (return the plain fmt error, the pre-TG-663 shape). RED: TestNativeRunnerLostAckAfterLaunchIsIndeterminate.
// Restored from a copy, green.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/config"
)

// tg663ServeDropAfterExec accepts the exec request (the command is LAUNCHED) and then drops the whole
// connection without ever sending an exit status — the lost-acknowledgement shape.
func tg663ServeDropAfterExec(t *testing.T, conn net.Conn, hostSigner cryptossh.Signer, wantClientPub cryptossh.PublicKey) {
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
	go cryptossh.DiscardRequests(reqs)
	for newCh := range chans {
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}
		for req := range chReqs {
			if req.Type == "exec" {
				_ = req.Reply(true, nil)
				_ = ch.Close()
				_ = sc.Close()
				return
			}
			_ = req.Reply(false, nil)
		}
	}
}

// tg663Runner mirrors tg80Runner with an injectable server behaviour and an optional WRONG host key in
// known_hosts (the handshake-refused pre-send case).
func tg663Runner(t *testing.T, wrongHostKey bool, serve func(net.Conn, cryptossh.Signer, cryptossh.PublicKey)) (*nativeRunner, []string) {
	t.Helper()
	hostSigner, _ := tg80GenSigner(t)
	clientSigner, clientPEM := tg80GenSigner(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "actuator")
	if err := os.WriteFile(keyPath, clientPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	pinned := hostSigner.PublicKey()
	if wrongHostKey {
		other, _ := tg80GenSigner(t)
		pinned = other.PublicKey()
	}
	khPath := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(khPath, []byte(knownhosts.Line([]string{"web01"}, pinned)+"\n"), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	r := &nativeRunner{
		knownHosts:     khPath,
		keyRef:         config.SecretRef("file:" + keyPath),
		connectTimeout: defaultConnectTimeout,
		dial: func(context.Context, string) (net.Conn, error) {
			c, s := tg80LoopbackPipe(t)
			go serve(s, hostSigner, clientSigner.PublicKey())
			return c, nil
		},
	}
	m := New("web01", "svc", &fakeRunner{})
	return r, m.sshArgv([]string{"systemctl", "start", "nginx"})
}

func TestNativeRunnerLostAckAfterLaunchIsIndeterminate(t *testing.T) {
	r, argv := tg663Runner(t, false, func(c net.Conn, hs cryptossh.Signer, pub cryptossh.PublicKey) { tg663ServeDropAfterExec(t, c, hs, pub) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := r.Run(ctx, argv, nil)
	if err == nil {
		t.Fatal("a connection dropped after launch must be an error")
	}
	if !errors.Is(err, actuation.ErrIndeterminate) {
		t.Fatalf("a Wait failure AFTER a successful Start must be INDETERMINATE (the command was launched), got %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrRemoteCancelled) {
		t.Fatalf("a lost acknowledgement is not a cancellation, got %v", err)
	}
}

// Pre-send failures stay plain: a host-key mismatch at the handshake, and a dial failure.
func TestNativeRunnerPreSendFailuresAreNotIndeterminate(t *testing.T) {
	r, argv := tg663Runner(t, true, func(c net.Conn, hs cryptossh.Signer, pub cryptossh.PublicKey) { tg663ServeDropAfterExec(t, c, hs, pub) })
	if _, err := r.Run(context.Background(), argv, nil); err == nil || errors.Is(err, actuation.ErrIndeterminate) {
		t.Fatalf("a handshake refused on the host key is a pre-send failure, never indeterminate, got %v", err)
	}
	r.dial = func(context.Context, string) (net.Conn, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
	if _, err := r.Run(context.Background(), argv, nil); err == nil || errors.Is(err, actuation.ErrIndeterminate) {
		t.Fatalf("a dial failure is a pre-send failure, never indeterminate, got %v", err)
	}
}

// A cancellation of a RUNNING command stays the TG-80 cancelled terminal AND is indeterminate.
func TestNativeRunnerCancelledRunIsAlsoIndeterminate(t *testing.T) {
	old := killGrace
	killGrace = 200 * time.Millisecond
	t.Cleanup(func() { killGrace = old })
	r, argv, _ := tg80Runner(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := r.Run(ctx, argv, nil)
	if !errors.Is(err, ErrRemoteCancelled) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the TG-80 classification must survive the wrap, got %v", err)
	}
	if !errors.Is(err, actuation.ErrIndeterminate) {
		t.Fatalf("a cancelled RUNNING command is indeterminate (its effect may have landed), got %v", err)
	}
}
