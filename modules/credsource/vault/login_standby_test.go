package vault

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// ★ THE LOGIN DRAWS A STANDBY EXACTLY LIKE EVERY OTHER REQUEST — AND IT WAS THE ONE PATH NOT RETRIED.
//
// THIRD control-plane outage from this cause, 2026-07-29, ~9 minutes: console/worker/grounder stuck in
// `Created` because tg-secretenv could not resolve LITELLM_MASTER_KEY and correctly failed closed.
//
// The 2026-07-27 and 2026-07-28 fixes retried the authenticated READ, and the budget was carefully sized
// from the real topology: one VIP round-robining three nodes, so p(standby) ≈ 2/3 per request, 19 attempts
// for (2/3)^19 ≈ 0.06%. But authed() resolves a token BEFORE entering that loop, and loginAt went straight
// to c.raw with no retry at all. The read budget was never reached. The tell was arithmetic: the init died
// 15 seconds in, not the ~30 seconds the budget implies.
//
// AND THE EXISTING SUITE COULD NOT SEE IT. Its fake Authenticator returns a token without any network call —
// its own comment says the tests exercise "the READ path (and only the read path)". A login that never
// happens cannot fail. That is why this file constructs a client whose Authenticator really logs in.
func TestTheLOGINSurvivesAStandbyRunToo(t *testing.T) {
	t.Setenv("TG_TEST_ROLE_ID", "r")
	t.Setenv("TG_TEST_SECRET_ID", "s")
	var loginCalls, standby int32
	// The first several login attempts hit a standby, exactly as the VIP would deal them. Sized to the
	// TEST budget (standby_test.go's init() shrinks retryBackoff to 3 so the suite runs fast) — the claim
	// here is that the login retries AT ALL, which it did not; the SHIPPED budget's adequacy is pinned
	// separately by TestTheRetryBudgetIsSizedForTheRealTopology.
	standbyRun := len(retryBackoff)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/approle/login" {
			if atomic.AddInt32(&loginCalls, 1) <= int32(standbyRun) {
				atomic.AddInt32(&standby, 1)
				w.WriteHeader(http.StatusTooManyRequests) // what a standby answers
				return
			}
			b, _ := json.Marshal(map[string]any{"auth": map[string]any{"client_token": "tok", "lease_duration": 3600}})
			_, _ = w.Write(b)
			return
		}
		_, _ = w.Write([]byte(kvBody()))
	}))
	defer ts.Close()

	c, err := New(Config{BaseURL: ts.URL, Auth: AppRole{RoleIDRef: "env:TG_TEST_ROLE_ID", SecretIDRef: "env:TG_TEST_SECRET_ID"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.ReadKV(context.Background(), "secret/data/tg/litellm")
	if err != nil {
		t.Fatalf("a %d-request standby run took the whole resolve down — the login is not retried, so the "+
			"read budget is never reached and the control plane strands in `Created`: %v", standbyRun, err)
	}
	if got["master_key"] != "sekrit" {
		t.Errorf("resolved the wrong value: %v", got)
	}
	if n := atomic.LoadInt32(&standby); n != int32(standbyRun) {
		t.Errorf("the fake served %d standby answers, expected %d — the test is not exercising what it claims", n, standbyRun)
	}
	if n := atomic.LoadInt32(&loginCalls); n <= int32(standbyRun) {
		t.Errorf("the login was attempted %d time(s), which is not more than the %d standby answers — it did "+
			"not retry", n, standbyRun)
	}
}

// FAIL-CLOSED IS UNCHANGED ON THE LOGIN PATH. Only 429/503 are transport conditions; a 403 is a VERDICT
// about the credential and must return immediately, not be hammered 19 times against a substrate that has
// already refused it.
func TestADeniedLoginIsNotRetried(t *testing.T) {
	t.Setenv("TG_TEST_ROLE_ID", "r")
	t.Setenv("TG_TEST_SECRET_ID", "s")
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()

	c, err := New(Config{BaseURL: ts.URL, Auth: AppRole{RoleIDRef: "env:TG_TEST_ROLE_ID", SecretIDRef: "env:TG_TEST_SECRET_ID"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.ReadKV(context.Background(), "secret/data/tg/litellm"); err == nil {
		t.Fatal("a 403 login succeeded — the connector must never fall open on a refused credential")
	}
	if n := atomic.LoadInt32(&calls); n > 2 {
		t.Errorf("a DENIED login was attempted %d times — 403 is a verdict, not a transport condition, and "+
			"retrying it hammers a substrate that has already said no", n)
	}
}
