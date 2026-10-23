package cisco

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"

	"github.com/territory-grounder/grounder/core/config"
)

// serveFakeBastion serves ONE SSH connection acting as a jump host: it authenticates the client, then honours
// "direct-tcpip" channel opens by connecting them to onwardAddr — the real ProxyJump mechanic, in process.
// It records each onward target it was asked for, so a test can prove the hop actually carried the device
// session rather than the client quietly dialling direct.
func serveFakeBastion(t *testing.T, conn net.Conn, hostSigner cryptossh.Signer, wantClientPub cryptossh.PublicKey, onward func() (net.Conn, error), asked chan<- string) {
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
	for newCh := range chans {
		if newCh.ChannelType() != "direct-tcpip" {
			_ = newCh.Reject(cryptossh.UnknownChannelType, "this bastion forwards only")
			continue
		}
		var payload struct {
			Host  string
			Port  uint32
			Orig  string
			OPort uint32
		}
		if err := cryptossh.Unmarshal(newCh.ExtraData(), &payload); err == nil {
			asked <- net.JoinHostPort(payload.Host, "22")
		}
		target, derr := onward()
		if derr != nil {
			_ = newCh.Reject(cryptossh.ConnectionFailed, derr.Error())
			continue
		}
		ch, chReqs, aerr := newCh.Accept()
		if aerr != nil {
			_ = target.Close()
			continue
		}
		go cryptossh.DiscardRequests(chReqs)
		// Splice the forwarded channel to the device connection — this is what a real bastion does.
		go copyBoth(ch, target)
	}
}

// copyBoth splices the forwarded channel to the device connection, both directions — what a real bastion does.
func copyBoth(a cryptossh.Channel, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	_ = a.Close()
	_ = b.Close()
}

// THE HOP CARRIES THE SESSION, AND BOTH PEERS ARE PINNED. The runner dials the bastion (verifying the
// bastion's own host key), the bastion opens the onward connection, and the DEVICE handshake — with the
// device's own host key — runs over that tunnel. The show output round-trips end to end.
func TestRunShowThroughAJumpHost(t *testing.T) {
	bastionSigner, _ := genSigner(t)
	deviceSigner, _ := genSigner(t)
	clientSigner, clientPEM := genSigner(t)
	t.Setenv("CISCO_TEST_KEY", string(clientPEM))

	// Each hop is pinned by its OWN known_hosts file — two independent verifications.
	bastionKH := writeKnownHosts(t, "bastion-dev:22", bastionSigner.PublicKey())
	deviceKH := writeKnownHosts(t, "cisco-dev:22", deviceSigner.PublicKey())

	gotCmds := make(chan string, 4)
	asked := make(chan string, 4)

	dev := Device{
		Host: "cisco-dev", Identity: "netops", KeyRef: config.SecretRef("env:CISCO_TEST_KEY"),
		KnownHosts: deviceKH,
		Jump: JumpHost{
			Host: "bastion-dev", Identity: "hopuser", KeyRef: config.SecretRef("env:CISCO_TEST_KEY"),
			KnownHosts: bastionKH,
		},
	}
	r := NewInteractiveRunner(dev)
	r.connectTimeout = 3 * time.Second
	r.ioTimeout = 3 * time.Second
	r.dial = func(_ context.Context, addr string) (net.Conn, error) {
		// Only the BASTION is dialled from TG — the device is reached through it.
		if !strings.HasPrefix(addr, "bastion-dev:") {
			t.Errorf("TG dialled %q directly; the hop was declared, so only the bastion may be dialled", addr)
		}
		client, server := loopbackPipe(t)
		go serveFakeBastion(t, server, bastionSigner, clientSigner.PublicKey(), func() (net.Conn, error) {
			dc, ds := loopbackPipe(t)
			go serveFakeCisco(t, ds, deviceSigner, clientSigner.PublicKey(), "Uptime: 3 days", gotCmds)
			return dc, nil
		}, asked)
		return client, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := New(r).Exec(ctx, []string{"show", "version"}, nil)
	if err != nil {
		t.Fatalf("show through the jump host: %v", err)
	}
	if !strings.Contains(string(out.Stdout), "Uptime: 3 days") {
		t.Errorf("device output did not round-trip through the hop: %q", out.Stdout)
	}
	select {
	case a := <-asked:
		if !strings.HasPrefix(a, "cisco-dev") {
			t.Errorf("the bastion was asked to reach %q, want the device", a)
		}
	default:
		t.Error("the bastion was never asked to open an onward connection — the hop did not carry the session")
	}
}

// A PARTIALLY declared hop REFUSES. It must never fall back to a direct dial: that would silently turn
// "reach the ASA through the bastion" into "reach whatever answers at that address".
func TestAPartialJumpHostRefusesAndNeverFallsBackToDirect(t *testing.T) {
	partial := map[string]JumpHost{
		"no identity":    {Host: "b", KeyRef: "env:K", KnownHosts: "/kh"},
		"no key_ref":     {Host: "b", Identity: "u", KnownHosts: "/kh"},
		"no known_hosts": {Host: "b", Identity: "u", KeyRef: "env:K"},
		"no host":        {Identity: "u", KeyRef: "env:K", KnownHosts: "/kh"},
	}
	for name, j := range partial {
		if !j.declared() {
			t.Fatalf("%s: the fixture must count as declared, else the case is vacuous", name)
		}
		if err := j.validate(); err == nil {
			t.Errorf("%s: a partially declared hop must refuse", name)
		}
	}
	// ...and the refusal reaches the transport: no dial is attempted at all.
	//
	// THE CREDENTIAL MUST RESOLVE for this to prove anything. Without it resolveSigner fails FIRST and the
	// dial never happens for an unrelated reason — the assertion below would then pass even if a partial hop
	// did fall back to a direct dial (measured: it did exactly that before this line was added).
	_, clientPEM := genSigner(t)
	t.Setenv("CISCO_TEST_KEY", string(clientPEM))
	dialed := 0
	dev := Device{
		Host: "cisco-dev", Identity: "netops", KeyRef: config.SecretRef("env:CISCO_TEST_KEY"),
		KnownHosts: writeKnownHosts(t, "cisco-dev:22", mustPub(t)),
		Jump:       JumpHost{Host: "bastion-dev"}, // declared, incomplete
	}
	r := NewInteractiveRunner(dev)
	r.dial = func(context.Context, string) (net.Conn, error) {
		dialed++
		return nil, errors.New("must not be reached")
	}
	if _, err := r.RunShow(context.Background(), "show version"); err == nil {
		t.Fatal("a partial hop must refuse the connection")
	}
	if dialed != 0 {
		t.Fatalf("a partial hop attempted %d dial(s) — it must not fall back to direct", dialed)
	}
}

// The zero value is a DIRECT dial: adding the field changes nothing for a device that needs no hop.
func TestNoJumpHostIsADirectDial(t *testing.T) {
	var none JumpHost
	if none.declared() {
		t.Fatal("the zero JumpHost must read as no hop at all")
	}
	r, _ := ciscoTestRunner(t, "Uptime: 1 day", false)
	if _, err := r.RunShow(context.Background(), "show version"); err != nil {
		t.Fatalf("a device with no hop must still dial directly: %v", err)
	}
}

func mustPub(t *testing.T) cryptossh.PublicKey {
	t.Helper()
	s, _ := genSigner(t)
	return s.PublicKey()
}

// AN UNPINNED BASTION IS REFUSED. The hop is a machine-in-the-middle, so it is authenticated exactly like the
// device: a bastion presenting a host key that is not in ITS known_hosts must fail the connection, and the
// device session must never start. KILLING MUTATION: drop verifier.Apply in dialThroughJump → this goes green
// on an unverified peer, which is the whole failure this pinning exists to prevent.
func TestAJumpHostWithAnUnpinnedKeyIsRefused(t *testing.T) {
	bastionSigner, _ := genSigner(t)
	otherSigner, _ := genSigner(t) // what known_hosts pins — NOT what the bastion presents
	deviceSigner, _ := genSigner(t)
	clientSigner, clientPEM := genSigner(t)
	t.Setenv("CISCO_TEST_KEY", string(clientPEM))

	dev := Device{
		Host: "cisco-dev", Identity: "netops", KeyRef: config.SecretRef("env:CISCO_TEST_KEY"),
		KnownHosts: writeKnownHosts(t, "cisco-dev:22", deviceSigner.PublicKey()),
		Jump: JumpHost{
			Host: "bastion-dev", Identity: "hopuser", KeyRef: config.SecretRef("env:CISCO_TEST_KEY"),
			KnownHosts: writeKnownHosts(t, "bastion-dev:22", otherSigner.PublicKey()),
		},
	}
	r := NewInteractiveRunner(dev)
	r.connectTimeout = 3 * time.Second
	r.ioTimeout = 3 * time.Second
	reached := 0
	r.dial = func(_ context.Context, _ string) (net.Conn, error) {
		client, server := loopbackPipe(t)
		go serveFakeBastion(t, server, bastionSigner, clientSigner.PublicKey(), func() (net.Conn, error) {
			reached++
			dc, ds := loopbackPipe(t)
			go serveFakeCisco(t, ds, deviceSigner, clientSigner.PublicKey(), "should never be read", make(chan string, 2))
			return dc, nil
		}, make(chan string, 2))
		return client, nil
	}
	if _, err := r.RunShow(context.Background(), "show version"); err == nil {
		t.Fatal("a bastion whose host key is not pinned must be refused")
	}
	if reached != 0 {
		t.Fatalf("the device was reached through an unverified bastion (%d onward dial(s))", reached)
	}
}
