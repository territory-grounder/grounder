package actuate

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeServiceHTTP(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer ok.Close()
	if got := ProbeService(context.Background(), ok.URL); got != ProbeUp {
		t.Fatalf("a server answering 404 is up, got %v", got)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer bad.Close()
	if got := ProbeService(context.Background(), bad.URL); got != ProbeDown {
		t.Fatalf("a server answering 502 is down, got %v", got)
	}
}

func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestProbeServiceRefusedIsDown(t *testing.T) {
	addr := closedPort(t)
	for _, target := range []string{"tcp://" + addr, "http://" + addr} {
		if got := ProbeService(context.Background(), target); got != ProbeDown {
			t.Fatalf("%s: got %v, want down", target, got)
		}
	}
}

func TestProbeServiceTCPConnectedIsUp(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if got := ProbeService(context.Background(), "tcp://"+l.Addr().String()); got != ProbeUp {
		t.Fatalf("got %v", got)
	}
}

func TestProbeServiceHungHTTPIsDown(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if got := ProbeService(ctx, "http://"+l.Addr().String()); got != ProbeDown {
		t.Fatalf("a service that accepts and never answers is hung, got %v", got)
	}
}

func TestProbeServiceIgnoresAProxyEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer ok.Close()
	if got := ProbeService(context.Background(), ok.URL); got != ProbeUp {
		t.Fatalf("a healthy service reached through a proxy variable read as %v", got)
	}
}

func TestProbeServiceMakesAFreshRequestEachTime(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	for i := 0; i < 2; i++ {
		if got := ProbeService(context.Background(), srv.URL); got != ProbeDown {
			t.Fatalf("sample %d: got %v", i, got)
		}
	}
	if hits != 2 {
		t.Fatalf("the server saw %d requests, want one per sample", hits)
	}
}

func TestProbeServiceHTTPSSelfSigned(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer srv.Close()
	if got := ProbeService(context.Background(), srv.URL); got != ProbeUp {
		t.Fatalf("a self-signed https server answering 404 is up, got %v", got)
	}
}

func TestProbeServiceWrongSchemeIsUnknownNotDown(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer plain.Close()
	target := "https://" + strings.TrimPrefix(plain.URL, "http://")
	if got := ProbeService(context.Background(), target); got != ProbeUnknown {
		t.Fatalf("an https probe of a plain http service is a wrong probe, not a fault; got %v", got)
	}
}

func TestProbeServiceUnparsableIsUnknown(t *testing.T) {
	for _, target := range []string{"", "tcp://", "ftp://" + strings.Repeat("a", 3) + ":1"} {
		if got := ProbeService(context.Background(), target); got != ProbeUnknown {
			t.Fatalf("%q: got %v", target, got)
		}
	}
}
