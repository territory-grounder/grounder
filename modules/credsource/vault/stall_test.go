package vault

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A REQUEST THAT NEVER GETS AN ANSWER MUST RE-ENTER THE SAME BOUNDED BUDGET AS A 429.
//
// This is the third time the retry budget in this package was never entered by the failure that actually
// happened. The file records two already (the read retried but the login did not; authed() resolved its
// token before the loop). The third took the operator console down on 2026-07-29:
//
//	browser sessions disabled: session key bao:secret/data/tg/session#key not resolvable
//	(vault: POST auth/cert/login: ... context deadline exceeded)
//
// isUnavailable matches ONLY a *statusError of 429/503. A timeout carries no status, so retrying() called
// do() exactly once and returned. The grounder disabled browser sessions, which un-registers /v1/session,
// and nobody could log in until a human restarted it — one unlucky request, one boot log line, no recovery.
//
// These oracles drive the REAL client against a substrate that STALLS (accepts the connection and never
// answers) rather than one that returns a status, because a fake that returns 503 would exercise the path
// that already worked and prove nothing about the one that failed.

// stallServer accepts requests and blocks until the test releases them, so the client's own per-request
// timeout fires — the production failure, not a simulated status code.
func stallServer(t *testing.T, stalls int, then http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var attempts atomic.Int32
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if int(n) <= stalls {
			select {
			case <-release:
			case <-r.Context().Done(): // the client gave up on this attempt — exactly the live condition
			}
			return
		}
		then(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, &attempts
}

// clientWithShortTimeout builds a client whose per-request timeout is small, so a stall resolves quickly.
func clientWithShortTimeout(t *testing.T, url string) *Client {
	t.Helper()
	c, err := New(Config{
		BaseURL:    url,
		Auth:       staticAuth{tok: "tok"},
		HTTPClient: &http.Client{Timeout: 120 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// THE DEFECT ITSELF: a stalled request must be retried, and the read must ultimately succeed.
func TestAStalledRequestReEntersTheRetryBudget(t *testing.T) {
	t.Parallel()
	stalls := len(retryBackoff) // the shrunk test budget; every attempt but the last stalls
	ts, attempts := stallServer(t, stalls, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(kvBody()))
	})
	c := clientWithShortTimeout(t, ts.URL)

	got, err := c.ReadKV(context.Background(), "secret/data/tg/session")
	if err != nil {
		t.Fatalf("a stalled request was not retried: %v\n"+
			"This is the console-login outage of 2026-07-29: a timeout carries no status, so isUnavailable "+
			"rejected it and the %d-attempt budget was skipped entirely.", err, len(retryBackoff))
	}
	if got["master_key"] != "sekrit" {
		t.Errorf("value = %q, want \"sekrit\"", got["master_key"])
	}
	if n := attempts.Load(); int(n) != stalls+1 {
		t.Errorf("attempts = %d, want %d (every stall retried, then one success)", n, stalls+1)
	}
}

// The same must hold for the LOGIN call, which is the one that actually failed live — the READ path already
// had a retry, and the outage was `POST auth/cert/login: context deadline exceeded`. This drives login
// through the client's own authenticator seam so it exercises the real retrying() wrapper around the login,
// not a hand-rolled imitation of it.
func TestAStalledLoginReEntersTheRetryBudget(t *testing.T) {
	t.Parallel()
	stalls := len(retryBackoff)
	ts, attempts := stallServer(t, stalls, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/login") {
			_, _ = w.Write([]byte(`{"auth":{"client_token":"t","lease_duration":3600,"renewable":true}}`))
			return
		}
		_, _ = w.Write([]byte(kvBody()))
	})
	c, err := New(Config{BaseURL: ts.URL, Auth: stallingLoginAuth{}, HTTPClient: &http.Client{Timeout: 120 * time.Millisecond}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.ReadKV(context.Background(), "secret/data/tg/session"); err != nil {
		t.Fatalf("a stalled LOGIN was not retried: %v — this is the exact call that failed live "+
			"(POST auth/cert/login: context deadline exceeded), and it is why the console could not be "+
			"logged into for 5 minutes", err)
	}
	if n := int(attempts.Load()); n < stalls+1 {
		t.Errorf("attempts = %d, want at least %d — the login did not re-enter the budget", n, stalls+1)
	}
}

// stallingLoginAuth performs a REAL http login against the test substrate through the client, so the stall
// happens inside the login request exactly as it did in production.
type stallingLoginAuth struct{}

func (stallingLoginAuth) login(ctx context.Context, c *Client) (string, time.Duration, bool, error) {
	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
			Renewable     bool   `json:"renewable"`
		} `json:"auth"`
	}
	raw, err := c.retrying(ctx, func() ([]byte, error) {
		return c.raw(ctx, http.MethodPost, "auth/cert/login", nil, "")
	})
	if err != nil {
		return "", 0, false, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", 0, false, err
	}
	return out.Auth.ClientToken, time.Duration(out.Auth.LeaseDuration) * time.Second, out.Auth.Renewable, nil
}

// FAIL-CLOSED IS UNCHANGED. Widening WHICH errors re-enter the loop must not widen what counts as success.
// A 403 and a 404 are decisions, not stalls, and must still fail on the first answer.
func TestADecisionIsNeverRetried(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		code int
	}{
		{"forbidden", http.StatusForbidden},
		{"not-found", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.WriteHeader(tc.code)
			}))
			defer ts.Close()
			c := newTestClient(t, ts)
			if _, err := c.ReadKV(context.Background(), "secret/data/tg/session"); err == nil {
				t.Fatal("a decision status returned no error — fail-closed is broken")
			}
			// 403 gets ONE re-login-and-retry by design (an expired token recovers); 404 gets none.
			// Either way it must be a small constant, never the retry budget.
			if n := int(attempts.Load()); n > 2 {
				t.Errorf("attempts = %d for %d — a decision entered the retry budget", n, tc.code)
			}
		})
	}
}

// THE PREDICATE ITSELF, over the closed set of error shapes this client can produce. The behavioural tests
// above prove the wiring; this proves the classification, including the two that must NOT be retryable.
func TestRetryablePredicateClassifiesEveryShape(t *testing.T) {
	t.Parallel()
	timeoutErr := &url.Error{Op: "Post", URL: "https://bao/v1/auth/cert/login", Err: context.DeadlineExceeded}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"429 standby", &statusError{status: http.StatusTooManyRequests}, true},
		{"503 unavailable", &statusError{status: http.StatusServiceUnavailable}, true},
		{"403 denied", &statusError{status: http.StatusForbidden}, false},
		{"404 absent", &statusError{status: http.StatusNotFound}, false},
		{"per-request timeout (THE outage)", timeoutErr, true},
		{"net timeout", &net.OpError{Op: "dial", Err: &timeoutOp{}}, true},
		{"plain error", errors.New("tls: bad certificate"), false},
		// ★ THE CASE THAT MAKES THE ORDERING INSIDE isTransportStall LOAD-BEARING. Deleting its
		// statusError early-return was a mutation control that would NOT go red, because no statusError
		// today also satisfies net.Error — the guard was correct and untested, which is the shape this
		// project has been bitten by repeatedly. This is the error a proxy or a wrapping transport can
		// really produce: a DECISION (403) carried inside something that reports Timeout()==true. Without
		// the early return it reads as a stall and a denial gets retried into the budget.
		{"403 wrapped in a timeout-reporting transport", timeoutWrapping{inner: &statusError{status: http.StatusForbidden}}, false},
		{"429 wrapped in a timeout-reporting transport", timeoutWrapping{inner: &statusError{status: http.StatusTooManyRequests}}, true},
	} {
		if got := retryable(tc.err); got != tc.want {
			t.Errorf("retryable(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// timeoutWrapping is a net.Error reporting Timeout()==true that WRAPS another error — the shape a proxy or
// an instrumented transport produces. It is the input that makes isTransportStall's statusError check
// falsifiable: the status underneath must win, so a decision is never mistaken for a stall.
type timeoutWrapping struct{ inner error }

func (t timeoutWrapping) Error() string   { return "transport timeout: " + t.inner.Error() }
func (t timeoutWrapping) Unwrap() error   { return t.inner }
func (t timeoutWrapping) Timeout() bool   { return true }
func (t timeoutWrapping) Temporary() bool { return true }

// timeoutOp is a net.Error whose Timeout() is true, for the predicate table above.
type timeoutOp struct{}

func (timeoutOp) Error() string   { return "i/o timeout" }
func (timeoutOp) Timeout() bool   { return true }
func (timeoutOp) Temporary() bool { return true }

// A CANCELLED CALLER MUST STOP THE LOOP, NOT EXTEND IT. context.DeadlineExceeded is retryable when it is
// the PER-REQUEST timeout, and must end the attempt when it is the CALLER's deadline — otherwise a caller
// that asked for a bounded wait gets an unbounded one. retrying() enforces this via its select on ctx.Done();
// this pins that the two cannot be confused.
func TestACancelledCallerStopsImmediately(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests) // always retryable
	}))
	defer ts.Close()
	c := newTestClient(t, ts)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead before the first attempt

	if _, err := c.ReadKV(ctx, "secret/data/tg/session"); err == nil {
		t.Fatal("a cancelled caller still returned a value")
	}
	if n := int(attempts.Load()); n > 1 {
		t.Errorf("attempts = %d with a cancelled context — the loop ignored the caller's cancellation", n)
	}
}
