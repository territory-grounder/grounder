package vault

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// kvBody is the shape ReadKV expects back from a KV-v2 read.
func kvBody() string {
	b, _ := json.Marshal(map[string]any{"data": map[string]any{"data": map[string]string{"master_key": "sekrit"}}})
	return string(b)
}

// staticAuth is an in-package Authenticator that hands back a fixed token without any network call, so
// these tests exercise the READ path (and only the read path) against the fake substrate.
type staticAuth struct{ tok string }

func (s staticAuth) login(context.Context, *Client) (string, time.Duration, bool, error) {
	return s.tok, time.Hour, false, nil
}

// newTestClient wires a Client at ts with a static token, bypassing login.
func newTestClient(t *testing.T, ts *httptest.Server) *Client {
	t.Helper()
	// Give this test client its OWN private http.Transport (TG-441). These tests run t.Parallel() and, because
	// ts.URL is http:// (no CA/cert), New would leave Transport nil ⇒ the process-wide http.DefaultTransport,
	// SHARED across every parallel test. httptest.Server.Close() explicitly calls
	// http.DefaultTransport.CloseIdleConnections() as a courtesy — so ONE parallel test's `defer ts.Close()`
	// poisons ANOTHER still-running test's in-flight request on that shared transport, surfacing as "connection
	// broken: CloseIdleConnections called": the attempt dies BEFORE reaching the server, so the server's call
	// counter never counts it. That intermittently breaks the retry-then-success oracle AND the exact-attempt-
	// count oracle (`want 4, made 3`) under -race in CI while passing locally — the TG-384 "green local, red CI"
	// shape. THE FIX is the PRIVATE transport instance: ts.Close() only ever reaches DefaultTransport, never a
	// client's own transport, so a private one cannot be poisoned by a sibling — do NOT "simplify" this back to
	// the default transport. DisableKeepAlives is belt-and-suspenders (no idle conn ever lingers to be closed),
	// not the load-bearing part. No production change: prod clients already build their own transport, and
	// nothing calls CloseIdleConnections on them mid-flight.
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	c, err := New(Config{BaseURL: ts.URL, Auth: staticAuth{tok: "tok"}, HTTPClient: hc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// prodRetryBackoff captures the SHIPPED budget before the init() below shrinks it for fast tests. Package
// variables are initialised before init() functions run, so this is the real production value.
//
// Without this the budget is untestable, and that is not a hypothetical: the 2026-07-27 fix shipped a
// 3-retry budget, every test in this file passed, and the same outage recurred on 2026-07-28. The existing
// oracles assert `len(retryBackoff)+1` attempts — which is TRUE for any budget, including one far too small
// to survive the cluster it talks to. They pin the mechanism and say nothing about whether it is enough.
var prodRetryBackoff = append([]time.Duration(nil), retryBackoff...)

func init() { retryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond} }

// TestTheRetryBudgetIsSizedForTheRealTopology is the oracle that the 2026-07-27 fix did not have.
//
// `openbao.example.net` is ONE VIP round-robining across three nodes. The leader serves; the two
// standbys answer 429 to a request they will not forward. Each attempt therefore draws a standby with
// p ≈ 2/3 INDEPENDENTLY, so the probability that a whole resolve fails is p^attempts. That is the number
// that decides whether a deploy strands the control plane, and it is the number this test pins.
func TestTheRetryBudgetIsSizedForTheRealTopology(t *testing.T) {
	t.Parallel()

	const pStandby = 2.0 / 3.0 // 2 of the 3 OpenBao nodes answer 429 rather than forwarding
	const maxAcceptableFailure = 0.001

	attempts := len(prodRetryBackoff) + 1 // the first request plus every retry
	failure := math.Pow(pStandby, float64(attempts))

	if failure > maxAcceptableFailure {
		t.Errorf("retry budget of %d attempts leaves a %.2f%% chance that a resolve fails against a "+
			"3-node VIP where 2 nodes answer 429 — every such failure strands litellm/worker/grounder/"+
			"console in `Created` and takes the control plane down until a human notices. Want <= %.2f%% "+
			"(needs about %d attempts). The 3-retry budget this replaced sat at 19.75%%, and it stranded "+
			"two deploys in two days.",
			attempts, failure*100, maxAcceptableFailure*100,
			int(math.Ceil(math.Log(maxAcceptableFailure)/math.Log(pStandby))))
	}

	// A budget can also fail by being unbounded-in-practice: a deploy that waits ten minutes for a substrate
	// that is genuinely down is its own outage. Keep the total wall-clock cost inside a deploy's patience.
	var total time.Duration
	for _, d := range prodRetryBackoff {
		total += d
	}
	if total > 90*time.Second {
		t.Errorf("total retry wait is %s — too long to sit on a deploy's critical path; a substrate that is "+
			"actually down must fail fast enough to be diagnosed, not hang the rollout", total)
	}
}

// TestStandby429IsRetriedThenSucceeds is the oracle for the 2026-07-27 control-plane outage.
//
// An OpenBao name fronts several nodes; a STANDBY answers 429 to a request it will not forward. Before this,
// a single unlucky standby hit made tg-secretenv fail closed, which left every container that depends_on it
// stuck in `Created` — the whole stack down. The secret existed and the token was authorised the entire time.
func TestStandby429IsRetriedThenSucceeds(t *testing.T) {
	t.Parallel()
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests) // standby: "I will not forward this"
			return
		}
		w.Write([]byte(kvBody()))
	}))
	defer ts.Close()

	got, err := newTestClient(t, ts).ReadKV(context.Background(), "secret/data/tg/litellm")
	if err != nil {
		t.Fatalf("a standby 429 must be retried, not fatal: %v", err)
	}
	if got["master_key"] != "sekrit" {
		t.Fatalf("resolved %v, want the secret after the standby cleared", got)
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("expected 2 retries then success (3 calls), got %d", n)
	}
}

// TestServiceUnavailable503IsRetried — a sealed/not-yet-ready node is the same class of transient.
func TestServiceUnavailable503IsRetried(t *testing.T) {
	t.Parallel()
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(kvBody()))
	}))
	defer ts.Close()

	if _, err := newTestClient(t, ts).ReadKV(context.Background(), "secret/data/x"); err != nil {
		t.Fatalf("503 must be retried: %v", err)
	}
}

// TestTerminalStatusesAreNeverRetried is the fail-closed half, and the more important one. A 404 and a 403
// are ANSWERS — "no such secret", "you may not read it". Retrying them would burn the deploy's budget on a
// verdict that will not change, and any future attempt to "recover" from them would be a fail-OPEN.
// Asserted over a CLOSED ENUMERATION of terminal statuses so a newly-retried status cannot slip in unseen.
func TestTerminalStatusesAreNeverRetried(t *testing.T) {
	t.Parallel()
	for _, code := range []int{
		http.StatusNotFound,            // the secret does not exist
		http.StatusBadRequest,          // malformed path
		http.StatusInternalServerError, // a real server fault, not a handoff
	} {
		var calls int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(code)
		}))
		_, err := newTestClient(t, ts).ReadKV(context.Background(), "secret/data/x")
		if err == nil {
			t.Fatalf("status %d must FAIL CLOSED, got nil error", code)
		}
		if n := atomic.LoadInt32(&calls); n != 1 {
			t.Errorf("status %d was retried %d time(s); terminal statuses must be attempted exactly once", code, n)
		}
		ts.Close()
	}
}

// TestExhaustedRetryBudgetStillFailsClosed — a substrate that is unavailable for good must still block the
// boot. The retry buys us a failover, never a secret we could not read.
func TestExhaustedRetryBudgetStillFailsClosed(t *testing.T) {
	t.Parallel()
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	if _, err := newTestClient(t, ts).ReadKV(context.Background(), "secret/data/x"); err == nil {
		t.Fatal("a permanently unavailable substrate must still fail closed")
	}
	if want := int32(len(retryBackoff) + 1); atomic.LoadInt32(&calls) != want {
		t.Fatalf("made %d attempts, want exactly %d (1 + bounded budget) — the retry must be BOUNDED",
			atomic.LoadInt32(&calls), want)
	}
}
