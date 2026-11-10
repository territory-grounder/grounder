package vault

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// noLeaderBody is OpenBao's answer from a standby during a leader election, as captured in production on
// 2026-11-07 (TG-651).
const noLeaderBody = `{"errors":["local node not active but active cluster node not found"]}`

// TestNoActiveNode500IsRetriedThenSucceeds is the oracle for TG-651: a 3-second OpenBao leader election
// failed a live triage, because the standby's 500 never entered the retry budget.
func TestNoActiveNode500IsRetriedThenSucceeds(t *testing.T) {
	t.Parallel()
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(noLeaderBody))
			return
		}
		w.Write([]byte(kvBody()))
	}))
	defer ts.Close()

	got, err := newTestClient(t, ts).ReadKV(context.Background(), "secret/data/tg/litellm")
	if err != nil {
		t.Fatalf("a no-active-node 500 (leader election) must be retried, not fatal: %v", err)
	}
	if got["master_key"] != "sekrit" {
		t.Fatalf("resolved %v, want the secret once the election settled", got)
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("expected 2 retries then success (3 calls), got %d", n)
	}
}

// TestOther500BodiesStayTerminal is the fail-closed half: only OpenBao's no-leader wording is a handoff. A 500
// with any other body is a real server fault and is attempted exactly once.
func TestOther500BodiesStayTerminal(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"errors":["internal error"]}`,
		`{"errors":["local node not active"]}`,
		`not json at all`,
	} {
		var calls int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(body))
		}))
		_, err := newTestClient(t, ts).ReadKV(context.Background(), "secret/data/x")
		if err == nil {
			t.Fatalf("500 %q must FAIL CLOSED, got nil error", body)
		}
		if n := atomic.LoadInt32(&calls); n != 1 {
			t.Errorf("500 %q was attempted %d time(s); a non-election 500 must be attempted exactly once", body, n)
		}
		ts.Close()
	}
}

// TestNoActiveNode500ExhaustedStillFailsClosed: an election that never settles must still fail closed after
// the bounded budget. The retry buys a failover, never a secret that could not be read.
func TestNoActiveNode500ExhaustedStillFailsClosed(t *testing.T) {
	t.Parallel()
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(noLeaderBody))
	}))
	defer ts.Close()

	if _, err := newTestClient(t, ts).ReadKV(context.Background(), "secret/data/x"); err == nil {
		t.Fatal("a cluster that never elects a leader must still fail closed")
	}
	if want := int32(len(retryBackoff) + 1); atomic.LoadInt32(&calls) != want {
		t.Fatalf("made %d attempts, want exactly %d (1 + bounded budget)", atomic.LoadInt32(&calls), want)
	}
}
