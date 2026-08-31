package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// A frozen clock. The refill IS the property that separates a brake from a lockout, so every oracle here has
// to be able to advance time deliberately rather than hope the wall clock cooperates.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 7, 29, 8, 0, 0, 0, time.UTC)} }
func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// brakeRouter builds a router whose single route answers with a caller-controlled status, through the REAL
// wrap() path. Asserting against the brake struct alone would prove the algorithm and nothing about whether
// any request in this program ever reaches it — the failure mode this codebase keeps re-finding.
func brakeRouter(t *testing.T, clk *fakeClock, status *int, served *int) *Router {
	t.Helper()
	rt := NewRouter(&Verifier{})
	rt.SetRejectBrake(newRejectBrake(clk.now))
	// brakeWrap is the production brake, over a handler whose status the oracle controls — an authenticated
	// route could only ever produce refusals, and half of what has to be proven here is what happens on a
	// SUCCESS. That wrap() really applies this to real routes is proven separately, below.
	rt.mux.Handle("/x/{id}", rt.brakeWrap(AuthHMAC, func(w http.ResponseWriter, r *http.Request) {
		*served++
		w.WriteHeader(*status)
	}))
	return rt
}

func do(rt *Router, method, path string, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "10.20.30.40:5555"
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: cookie})
	}
	w := httptest.NewRecorder()
	rt.Mux().ServeHTTP(w, req)
	return w
}

// THE INCIDENT, REPRODUCED AND CAPPED.
//
// Measured live 2026-07-28: one idle console tab on a run whose /v1/sessions/{ref} answered 403 produced
// 28,094 rejected requests against 1 success, ~75 req/s sustained, through the nginx that also carries the
// LibreNMS and Alertmanager ingest lanes. This drives the same shape — a client that never pauses and never
// succeeds — and asserts the server stops doing the work.
func TestASustainedRejectionStormIsCappedAtTheRouteBudget(t *testing.T) {
	clk := newClock()
	status, served := http.StatusForbidden, 0
	rt := brakeRouter(t, clk, &status, &served)

	braked := 0
	for range 3000 { // the storm: no delay between requests, exactly as observed
		if do(rt, http.MethodGet, "/x/run-1", "tab-A").Code == http.StatusTooManyRequests {
			braked++
		}
	}
	if served > brakeRouteCap {
		t.Errorf("the handler ran %d times for 3000 refused requests — the brake did not engage; the "+
			"budget is %d", served, brakeRouteCap)
	}
	if braked < 2900 {
		t.Errorf("only %d of 3000 storm requests were short-circuited", braked)
	}
	// The shape of the win, stated as the incident would have measured it: 28,094 requests would have cost
	// at most brakeRouteCap dispatches instead of 28,094.
	if served == 0 {
		t.Error("ZERO requests were dispatched — the brake must let the budget through before engaging, or " +
			"it is a blanket denial and the first legitimate request never runs")
	}
}

// ★ THE PROPERTY THAT KEEPS THIS FROM BEING AN OUTAGE. The brake is a pause on a clock, not a verdict. If the
// model behind it is WRONG — if the 21st request would in fact have succeeded — the damage must be bounded by
// seconds, not by the lifetime of the process.
func TestTheBrakeIsNeverPermanent(t *testing.T) {
	clk := newClock()
	status, served := http.StatusForbidden, 0
	rt := brakeRouter(t, clk, &status, &served)

	for range 500 {
		do(rt, http.MethodGet, "/x/run-1", "tab-A")
	}
	if got := do(rt, http.MethodGet, "/x/run-1", "tab-A").Code; got != http.StatusTooManyRequests {
		t.Fatalf("expected the brake to be engaged, got %d", got)
	}
	before := served
	clk.add(3 * time.Second) // one route token refills every two seconds
	if got := do(rt, http.MethodGet, "/x/run-1", "tab-A").Code; got == http.StatusTooManyRequests {
		t.Fatal("still braked after the refill interval — a brake that does not reopen on a clock is a " +
			"lockout, and an operator whose role was granted a second ago would be locked out of the exact " +
			"surface they were granted")
	}
	if served != before+1 {
		t.Errorf("the refilled request did not reach the handler (served %d -> %d)", before, served)
	}
}

// A SUCCESS RESTORES THE ROUTE IN FULL, IMMEDIATELY. This is the converse of the storm test and it is the one
// that matters for operator safety: the moment the server proves it will answer this route, the brake must
// stop holding the earlier refusals against it. Without this, a role granted mid-incident would leave the
// operator crawling at the refill rate for the next minute.
func TestOneSuccessClearsTheWholeRouteBudget(t *testing.T) {
	clk := newClock()
	status, served := http.StatusForbidden, 0
	rt := brakeRouter(t, clk, &status, &served)

	for range brakeRouteCap - 1 { // spend all but one token
		do(rt, http.MethodGet, "/x/run-1", "tab-A")
	}
	status = http.StatusOK // the operator's situation changed
	if got := do(rt, http.MethodGet, "/x/run-1", "tab-A").Code; got != http.StatusOK {
		t.Fatalf("the success was refused with %d", got)
	}
	status = http.StatusForbidden
	before := served
	for range brakeRouteCap {
		do(rt, http.MethodGet, "/x/run-1", "tab-A")
	}
	if served != before+brakeRouteCap {
		t.Errorf("after a success the route dispatched %d of %d further requests — the budget was not "+
			"restored, so a client that is making progress is still being charged for refusals it has "+
			"already recovered from", served-before, brakeRouteCap)
	}
}

// A CLIENT MAKING PROGRESS AT HUMAN SPEED IS NEVER BRAKED, FOR AS LONG AS IT RUNS. Interleaving successes
// with rejections is what a healthy console does — it polls a dozen endpoints and some of them 403 for an
// operator without the elevated role. Four requests a second is already brisk for a browser on a timer.
//
// ★ THIS ORACLE WAS RED WHEN FIRST WRITTEN, AND IT WAS RIGHT TO BE. The first draft ran the loop on a FROZEN
// clock and braked at iteration 119 — the per-client bucket, which time-refills only. That is not a defect at
// 120 rejections in zero elapsed time (that is a storm however many successes are mixed in), but writing the
// loop that way meant the oracle asserted nothing about the case that actually matters. The fix was to make
// the traffic PHYSICAL, and to bound the zero-time case separately in the test below.
func TestAClientAlternatingAtHumanSpeedIsNeverBraked(t *testing.T) {
	clk := newClock()
	status, served := http.StatusOK, 0
	rt := brakeRouter(t, clk, &status, &served)

	for i := range 4000 {
		clk.add(250 * time.Millisecond) // 4 req/s: two rejections a second, under the 4/s client refill
		status = http.StatusForbidden
		if do(rt, http.MethodGet, "/x/run-1", "tab-A").Code == http.StatusTooManyRequests {
			t.Fatalf("braked at iteration %d despite a success between every rejection", i)
		}
		clk.add(250 * time.Millisecond)
		status = http.StatusOK
		if do(rt, http.MethodGet, "/x/run-1", "tab-A").Code == http.StatusTooManyRequests {
			t.Fatalf("braked a request that was going to SUCCEED, at iteration %d", i)
		}
	}
}

// ★ THE COST OF THE CLIENT-LEVEL BRAKE, MEASURED AND BOUNDED. Unlike the route level, the client level does
// NOT refill on success — it cannot, or a storming tab's own healthy polling would keep it topped up and the
// rotation guard would be armed and unable to fire. The price is that a client which has just spent 120
// rejections faster than they refill CAN have a would-succeed request refused. That harm is real, so it has
// to be bounded rather than argued away: it must clear within one refill tick.
func TestTheClientLevelBrakeReleasesWithinOneRefillTick(t *testing.T) {
	clk := newClock()
	status, served := http.StatusForbidden, 0
	rt := brakeRouter(t, clk, &status, &served)

	for i := range brakeClientCap + 20 { // drain the client bucket across fresh routes, at network speed
		do(rt, http.MethodGet, fmt.Sprintf("/x/run-%d", i), "tab-A")
	}
	status = http.StatusOK
	if do(rt, http.MethodGet, "/x/fresh", "tab-A").Code != http.StatusTooManyRequests {
		t.Fatal("the client bucket is not drained; the premise of this test does not hold")
	}
	clk.add(time.Second / brakeClientRefill) // exactly one token
	if got := do(rt, http.MethodGet, "/x/fresh", "tab-A").Code; got != http.StatusOK {
		t.Errorf("a would-succeed request was still refused (%d) one refill tick after the client bucket "+
			"drained — the client level must cost at most %v of latency, never a lockout",
			got, time.Second/brakeClientRefill)
	}
}

// A STORM ON ONE ROUTE MUST NOT SILENCE ANOTHER. The console loads a dozen surfaces; braking all of them
// because one run's detail is refused would turn a partial defect into a blank console.
func TestARouteStormDoesNotBrakeADifferentRoute(t *testing.T) {
	clk := newClock()
	status, served := http.StatusForbidden, 0
	rt := brakeRouter(t, clk, &status, &served)

	for range 200 {
		do(rt, http.MethodGet, "/x/run-1", "tab-A")
	}
	if do(rt, http.MethodGet, "/x/run-1", "tab-A").Code != http.StatusTooManyRequests {
		t.Fatal("route one is not braked; the premise of this test does not hold")
	}
	status = http.StatusOK
	if got := do(rt, http.MethodGet, "/x/run-2", "tab-A").Code; got != http.StatusOK {
		t.Errorf("a DIFFERENT route answered %d while run-1 was braked — one refused surface must not take "+
			"the rest of the console down with it", got)
	}
}

// ONE OPERATOR'S STORM MUST NOT BRAKE ANOTHER OPERATOR. Behind a reverse proxy every console client shares a
// peer address, so identity has to come from the session cookie or the brake becomes collateral damage.
func TestOneOperatorsStormDoesNotBrakeAnother(t *testing.T) {
	clk := newClock()
	status, served := http.StatusForbidden, 0
	rt := brakeRouter(t, clk, &status, &served)

	for range 500 {
		do(rt, http.MethodGet, "/x/run-1", "operator-one-session")
	}
	if do(rt, http.MethodGet, "/x/run-1", "operator-one-session").Code != http.StatusTooManyRequests {
		t.Fatal("operator one is not braked; the premise does not hold")
	}
	status = http.StatusOK
	if got := do(rt, http.MethodGet, "/x/run-1", "operator-two-session").Code; got != http.StatusOK {
		t.Errorf("operator two got %d on the same route — both arrive from the same proxy address, so "+
			"keying the brake on the peer address alone would lock out every other operator in the estate", got)
	}
}

// THE ROTATION CASE the per-route level cannot see. Nothing about the observed cycle required a single path;
// a client that walks 200 sessions would otherwise spend 200x the route budget.
func TestAPathRotatingStormIsCappedByTheClientLevel(t *testing.T) {
	clk := newClock()
	status, served := http.StatusForbidden, 0
	rt := brakeRouter(t, clk, &status, &served)

	for i := range 1000 { // a fresh path every request: the route bucket never fills up
		do(rt, http.MethodGet, fmt.Sprintf("/x/run-%d", i), "tab-A")
	}
	if served > brakeClientCap {
		t.Errorf("a path-rotating storm dispatched %d requests — the per-client budget is %d, and without "+
			"it the per-route brake is trivially defeated by changing the path", served, brakeClientCap)
	}
}

// ★ THE REAL INCIDENT SHAPE: THE STORMING TAB WAS ALSO SUCCEEDING ELSEWHERE.
//
// The console polls a dozen endpoints on a timer, so the tab that produced 28,094 rejections was at the same
// time getting 200s from /v1/stats, /v1/alerts and the rest. If a success anywhere refilled the CLIENT bucket,
// the rotation guard would be permanently topped up by the storm's own healthy polling — present, reachable,
// and unable to ever reach the state it guards. That is the defect shape this codebase keeps re-finding, and
// without this oracle the claim lives only in a comment.
func TestSuccessesOnAHEALTHYROUTEDoNotRefuelARotatingStorm(t *testing.T) {
	clk := newClock()
	status, served := http.StatusForbidden, 0
	rt := brakeRouter(t, clk, &status, &served)

	stormed := 0
	for i := range 1000 {
		status = http.StatusForbidden
		if do(rt, http.MethodGet, fmt.Sprintf("/x/run-%d", i), "tab-A").Code != http.StatusTooManyRequests {
			stormed++
		}
		status = http.StatusOK // the same tab's healthy polling, interleaved exactly as in production
		do(rt, http.MethodGet, "/x/stats", "tab-A")
	}
	if stormed > brakeClientCap {
		t.Errorf("the rotating storm got %d requests through (client budget %d) while its own successful "+
			"polling ran alongside it — a success must not refuel the per-client bucket, or the guard is "+
			"armed and can never fire", stormed, brakeClientCap)
	}
}

// ★ THE EXEMPTION IS THE SAFETY ARGUMENT. Login and step-up elevation are how a caller STOPS being rejected.
// Braking them would convert a recoverable state into a lockout: a client that spent its budget on refused
// reads could not authenticate its way out.
func TestTheLoginAndElevationRoutesAreNeverBraked(t *testing.T) {
	for _, m := range []AuthMethod{AuthOperatorLogin, AuthAdminElevate} {
		if brakeApplies(m) {
			t.Errorf("auth method %v is braked — it is the route a rejected caller uses to become "+
				"accepted, so braking it turns a refusal into a lockout", m)
		}
	}
	for _, m := range []AuthMethod{AuthMTLS, AuthHMAC, AuthReadOnly, AuthSession, AuthTraceRead, AuthAdminSession, AuthIngestPush} {
		if !brakeApplies(m) {
			t.Errorf("auth method %v is EXEMPT from the brake — the storm arrived on a read route, so an "+
				"exemption here silently reopens the incident", m)
		}
	}
}

// The identity must not be client-choosable. Honouring X-Forwarded-For would let any caller mint a fresh
// brake identity per request and opt out of the control entirely.
func TestTheBrakeIdentityIsNotClientChoosable(t *testing.T) {
	a := httptest.NewRequest(http.MethodGet, "/x/1", nil)
	a.RemoteAddr = "10.1.1.1:9"
	a.Header.Set("X-Forwarded-For", "1.2.3.4")
	b := httptest.NewRequest(http.MethodGet, "/x/1", nil)
	b.RemoteAddr = "10.1.1.1:9"
	b.Header.Set("X-Forwarded-For", "5.6.7.8")
	if brakeClientKey(a) != brakeClientKey(b) {
		t.Error("two requests from the same peer got different brake identities because they claimed " +
			"different X-Forwarded-For values — a client-supplied identity is an opt-out")
	}
	c := httptest.NewRequest(http.MethodGet, "/x/1", nil)
	c.RemoteAddr = "10.1.1.1:9"
	c.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "sess"})
	if brakeClientKey(c) == brakeClientKey(a) {
		t.Error("a session-bearing request shares an identity with an anonymous one from the same address")
	}
	if k := brakeClientKey(c); len(k) < 3 || k[:2] != "s:" {
		t.Errorf("session identity %q is not derived from the cookie", k)
	}
	// The cookie VALUE must never appear in the key — brake keys are held in memory and printed in
	// diagnostics; a session token is a bearer credential.
	if k := brakeClientKey(c); k == "s:sess" || len(k) > 2 && k[2:] == "sess" {
		t.Error("the raw session cookie value is the brake key — that is a bearer credential in a map key")
	}
}

// The map is bounded. Keying on a client-supplied path means an unbounded key space; without eviction the
// brake becomes the memory exhaustion it exists to prevent.
func TestTheBrakeMapIsBounded(t *testing.T) {
	clk := newClock()
	rb := newRejectBrake(clk.now)
	for i := range brakeMaxEntries * 3 {
		p := fmt.Sprintf("/x/%d", i)
		rb.Allow("a:1.2.3.4", http.MethodGet, p)
		rb.Record("a:1.2.3.4", http.MethodGet, p, http.StatusForbidden)
	}
	rb.mu.Lock()
	n := len(rb.routes)
	rb.mu.Unlock()
	if n > brakeMaxEntries {
		t.Errorf("the route map holds %d entries, cap is %d — an attacker choosing paths could grow it "+
			"without bound", n, brakeMaxEntries)
	}
}

// SSE must keep flushing. The live decision tracer streams over /v1/sessions/{ref}/stream; a response wrapper
// that swallowed Flush would leave the page permanently blank and nothing else in the path would report it.
func TestTheBrakeWrapperStillFlushes(t *testing.T) {
	rec := httptest.NewRecorder()
	b := &brakeRecorder{ResponseWriter: rec}
	if _, ok := any(b).(http.Flusher); !ok {
		t.Fatal("brakeRecorder is not an http.Flusher — SSE handlers type-assert for it and silently stop " +
			"streaming when the assertion fails")
	}
	b.WriteHeader(http.StatusOK)
	if _, err := b.Write([]byte("event: snapshot\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	b.Flush()
	if !rec.Flushed {
		t.Error("Flush did not reach the underlying writer")
	}
}

// ★ REACHABILITY. Every oracle above drives brakeWrap directly. This one drives a route registered exactly
// the way production registers one — rt.Handle with a real auth method — and proves the brake is on the path
// a request actually takes. Without it the whole file could pass over a seam nothing calls, which is the
// defect shape this project keeps re-finding: implemented, tested, and not reachable.
func TestEveryBrakedAuthMethodActuallyReachesTheBrake(t *testing.T) {
	// AuthMTLS refuses an unauthenticated request with 401 — a real refusal from the real dispatcher, so the
	// storm below is indistinguishable from the production one.
	for _, m := range []AuthMethod{AuthMTLS, AuthHMAC, AuthReadOnly, AuthSession, AuthTraceRead, AuthAdminSession, AuthIngestPush} {
		t.Run(fmt.Sprintf("method-%v", m), func(t *testing.T) {
			rt := NewRouter(&Verifier{})
			rt.Handle("/real", m, func(w http.ResponseWriter, r *http.Request, p Principal) {
				w.WriteHeader(http.StatusOK)
			}, http.MethodGet)
			last := 0
			for range brakeRouteCap + 5 {
				last = do(rt, http.MethodGet, "/real", "").Code
			}
			if last != http.StatusTooManyRequests {
				t.Errorf("after %d refusals a real %v route still answered %d — wrap() is not applying the "+
					"brake, so every oracle in this file is testing an unreachable seam",
					brakeRouteCap+5, m, last)
			}
		})
	}
	// And the exempt ones must survive the same storm.
	for _, m := range []AuthMethod{AuthOperatorLogin, AuthAdminElevate} {
		rt := NewRouter(&Verifier{})
		rt.Handle("/login", m, func(w http.ResponseWriter, r *http.Request, p Principal) {}, http.MethodPost)
		last := 0
		for range brakeRouteCap * 3 {
			last = do(rt, http.MethodPost, "/login", "").Code
		}
		if last == http.StatusTooManyRequests {
			t.Errorf("auth method %v was braked on a real route — a caller who exhausted their budget on "+
				"refused reads could no longer authenticate their way out", m)
		}
	}
}

// A handler that sets no status sent 200, and must be recorded as a success — otherwise every empty-bodied
// 200 would be charged as a rejection and healthy traffic would brake itself.
func TestAnUnsetStatusIsRecordedAsSuccess(t *testing.T) {
	b := &brakeRecorder{ResponseWriter: httptest.NewRecorder()}
	if got := b.statusOrOK(); got != http.StatusOK {
		t.Errorf("an unset status recorded as %d — Go sends 200 when a handler writes nothing", got)
	}
}
