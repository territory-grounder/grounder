package ssh

import (
	"context"
	"net"
	"strings"
	"testing"
)

// TG-81 b4: the ARMED pre-flight probe proves the TRANSPORT and nothing else — a live listener answers
// true, a dead endpoint answers false with the dial detail, and no command travels either way (the
// runner is a fake). The dial seam is injected exactly as production injects the TCP dialer, pointed at
// a listener this test owns. KILLING MUTATION: make ProbeReachable return (true, "") without dialing —
// the dead-endpoint case fails.
func TestProbeReachableProvesTheTransportOnly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	lnAddr := ln.Addr().String()

	d := net.Dialer{}
	m := New("web01", "svc-agent", &fakeRunner{},
		WithReachabilityProbe(func(ctx context.Context, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp", lnAddr) // the oracle's endpoint stands in for host:22
		}))
	if ok, detail := m.ProbeReachable(context.Background(), "web01"); !ok {
		t.Fatalf("a live listener must probe reachable, got %q", detail)
	}

	ln.Close() // the endpoint dies; the probe must say so, with the dial detail
	if ok, detail := m.ProbeReachable(context.Background(), "web01"); ok {
		t.Fatal("a dead endpoint must probe unreachable")
	} else if !strings.Contains(detail, "tcp dial") {
		t.Fatalf("the refusal must carry the dial detail, got %q", detail)
	}
}

// UNARMED, the probe passes through HONESTLY: true, with a detail saying the transport was not proven —
// never a silent pass that reads like a dialed probe. This is what keeps every fake-runner oracle and
// read-only construction network-free.
func TestUnarmedProbeIsAnHonestPassThrough(t *testing.T) {
	m := New("web01", "svc-agent", &fakeRunner{})
	ok, detail := m.ProbeReachable(context.Background(), "web01")
	if !ok {
		t.Fatal("unarmed must pass through, not refuse")
	}
	if !strings.Contains(detail, "not proven") {
		t.Fatalf("the pass-through must say the transport was not proven, got %q", detail)
	}
}
