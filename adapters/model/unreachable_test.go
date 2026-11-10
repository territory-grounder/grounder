package model

// TG-656: a gateway that cannot be DIALLED (litellm recreated by a deploy, or its name not resolving for a
// moment) is retried at the call level with a bounded schedule; a call that reached the gateway is not. Seen
// live on 2026-11-07 13:14 NL: a deploy recreated litellm and the triage in flight failed on "connection
// refused", losing a G3 heal. As in ratelimit_test.go, the load-bearing assertion is the ATTEMPT count.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// prodGatewayUnreachableBackoff is the SHIPPED schedule, captured before init() shrinks it (package variables
// initialise before init functions run).
var prodGatewayUnreachableBackoff = append([]time.Duration(nil), gatewayUnreachableBackoff...)

func init() {
	gatewayUnreachableBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
}

// dialFailRT fails its first `fail` round trips with `failErr` (as the transport would), then delegates.
type dialFailRT struct {
	fail    int64
	failErr error
	calls   atomic.Int64
	next    http.RoundTripper
}

func (d *dialFailRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if d.calls.Add(1) <= d.fail {
		return nil, d.failErr
	}
	return d.next.RoundTrip(r)
}

func refusedErr() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
}

func okServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, okCompletionBody)
	}))
}

func TestGatewayUnreachableIsRiddenOut(t *testing.T) {
	for name, failErr := range map[string]error{
		"connection refused": refusedErr(),
		"dns miss":           &net.DNSError{Err: "no such host", Name: "litellm", IsNotFound: true},
	} {
		t.Run(name, func(t *testing.T) {
			srv := okServer()
			defer srv.Close()
			obs := &recObs{}
			g := retryGateway(t, srv.URL, obs)
			rt := &dialFailRT{fail: 3, failErr: failErr, next: http.DefaultTransport}
			g.HTTP = &http.Client{Transport: rt}
			out, err := g.Complete(context.Background(), "u", "primary", msgs())
			if err != nil || out != "hello" {
				t.Fatalf("an unreachable gateway that comes back must be ridden out; out=%q err=%v", out, err)
			}
			if got := rt.calls.Load(); got != 4 {
				t.Fatalf("want 4 attempts (3 failed dials + 1 success), got %d", got)
			}
			if got := obs.count("transport_retry"); got != 3 {
				t.Fatalf("want 3 transport_retry observations, got %d (%v)", got, obs.outcomes)
			}
			// The ridden-out dials must NOT accrue: the threshold-1 breaker stays closed, so a second call works.
			if _, err := g.Complete(context.Background(), "u", "primary", msgs()); err != nil {
				t.Fatalf("ridden-out dial failures must not trip the breaker: %v", err)
			}
		})
	}
}

func TestGatewayUnreachableThatStaysDownFailsAfterTheBoundedSchedule(t *testing.T) {
	obs := &recObs{}
	g := retryGateway(t, "http://gateway.invalid", obs)
	rt := &dialFailRT{fail: 1 << 30, failErr: refusedErr(), next: http.DefaultTransport}
	g.HTTP = &http.Client{Transport: rt}
	_, err := g.Complete(context.Background(), "u", "primary", msgs())
	var me *ModelError
	if !errors.As(err, &me) || me.Class != "transport" {
		t.Fatalf("a gateway that stays down must surface as transport, got %v", err)
	}
	if want := int64(len(gatewayUnreachableBackoff) + 1); rt.calls.Load() != want {
		t.Fatalf("made %d attempts, want exactly %d (1 + bounded schedule)", rt.calls.Load(), want)
	}
}

// A failure AFTER the request may have reached the gateway (a reset, an unexpected EOF) is not retried here:
// it may have reached a model, so it keeps the single-attempt handling it always had.
func TestGatewayFailureAfterSendIsNotRetried(t *testing.T) {
	for name, failErr := range map[string]error{
		"read reset": &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)},
		"eof":        io.ErrUnexpectedEOF,
	} {
		t.Run(name, func(t *testing.T) {
			obs := &recObs{}
			g := retryGateway(t, "http://gateway.invalid", obs)
			rt := &dialFailRT{fail: 1 << 30, failErr: failErr, next: http.DefaultTransport}
			g.HTTP = &http.Client{Transport: rt}
			if _, err := g.Complete(context.Background(), "u", "primary", msgs()); err == nil {
				t.Fatal("must fail")
			}
			if got := rt.calls.Load(); got != 1 {
				t.Fatalf("a failure after send must be attempted exactly once, got %d", got)
			}
			if got := obs.count("transport_retry"); got != 0 {
				t.Fatalf("no transport_retry for a failure after send, got %d", got)
			}
		})
	}
}

// The shipped schedule rides out a ~20 s gateway restart but surfaces a dead gateway in well under a minute.
func TestGatewayUnreachableScheduleIsSizedForARestart(t *testing.T) {
	var total time.Duration
	for _, d := range prodGatewayUnreachableBackoff {
		total += d
	}
	if total < 20*time.Second || total > 40*time.Second {
		t.Fatalf("unreachable retry schedule totals %s; want 20-40 s (ride out a deploy's litellm recreate, surface a dead gateway fast)", total)
	}
}
