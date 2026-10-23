package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"sync"
	"time"
)

// REQ-528 — THE REPEATED-REJECTION BRAKE.
//
// A client that has been told "no" many times in a row, without a single success in between, is not making
// progress. Answering the next identical request from memory costs nothing that was going to be granted.
//
// This exists because of a MEASURED production incident (2026-07-28). The console's Workflows detail load was
// an unbounded retry cycle: the catch cleared its loading flag and re-rendered, the re-render re-invoked the
// select hook, and the guard let it straight back through — nothing in the loop waited, so it spun at network
// speed. One idle tab, on a run whose /v1/sessions/{ref} answered 403, produced **28,094 rejected requests
// against 1 success — ~75 req/s sustained for hours**, through the same nginx that carries the LibreNMS and
// Alertmanager ingest lanes. A remediation control plane was being denied-of-service by its own console.
//
// ★ THE CLIENT-SIDE LATCH SHIPPED FIRST AND IS NOT ENOUGH. That fix lives in JavaScript the browser already
// downloaded. Every tab opened before the deploy is still running the OLD loop, and no amount of server-side
// correctness reaches it — the server cannot make a stale client stop asking. It can only stop answering
// expensively. That is the whole job of this file: it is a backstop for a bug that is already fixed, in
// clients that will never receive the fix.
//
// WHAT IT DELIBERATELY IS NOT: a rate limiter. A rate limiter caps SUCCESSFUL work and would be a real risk
// to an operator acting during an incident. This caps REJECTED work only. Tokens are consumed by responses
// that already refused the caller; a 2xx consumes nothing and immediately restores that route's full budget.
// The strongest statement it can make about a request it short-circuits is that the previous N identical
// requests were all refused, and that its own refill lets the next one through within seconds regardless.

const (
	// brakeRouteCap is how many rejections one (client, method, path) may spend before the brake engages.
	// A human cannot produce twenty rejections on ONE route without noticing; the storm produced 28,094.
	brakeRouteCap = 20
	// brakeRouteRefill is tokens per second for that route. It bounds the storm to ~1 request every two
	// seconds — a 150x reduction on the measured rate — and, far more importantly, bounds how long a client
	// whose situation genuinely CHANGED has to wait before it is heard again.
	brakeRouteRefill = 0.5

	// brakeClientCap / brakeClientRefill are the second level, and they exist for a storm the first level
	// cannot see: a client that rotates paths. The measured incident hammered one path, but nothing about the
	// cycle required that, and a per-route brake alone would let a rotation over 200 sessions spend 200x the
	// route budget. Sized so no human-driven session can reach it: four rejections a second, sustained.
	brakeClientCap    = 120
	brakeClientRefill = 4

	// brakeMaxEntries bounds the map itself. Keying on a client-supplied path means an attacker could
	// otherwise mint unbounded keys — the brake would become the memory exhaustion it exists to prevent.
	brakeMaxEntries = 4096
)

// brakeBucket is a token bucket that refills with time. Tokens are spent ONLY by rejections.
type brakeBucket struct {
	tokens float64
	last   time.Time
	cap    float64
	refill float64
}

func (b *brakeBucket) advance(now time.Time) {
	if !b.last.IsZero() {
		if d := now.Sub(b.last).Seconds(); d > 0 {
			b.tokens += d * b.refill
			if b.tokens > b.cap {
				b.tokens = b.cap
			}
		}
	}
	b.last = now
}

// RejectBrake is the shared state. The zero value is not usable; build it with newRejectBrake.
type RejectBrake struct {
	mu      sync.Mutex
	now     func() time.Time
	routes  map[string]*brakeBucket
	clients map[string]*brakeBucket
}

func newRejectBrake(now func() time.Time) *RejectBrake {
	if now == nil {
		now = time.Now
	}
	return &RejectBrake{now: now, routes: map[string]*brakeBucket{}, clients: map[string]*brakeBucket{}}
}

// NewRejectBrakeForTest builds a brake on an injected clock. Exported for oracles only: a brake tested on the
// wall clock would either take minutes to run or assert nothing about refill, and the refill IS the property
// that keeps this from becoming a lockout.
func NewRejectBrakeForTest(now func() time.Time) *RejectBrake { return newRejectBrake(now) }

func (rb *RejectBrake) bucket(m map[string]*brakeBucket, key string, capacity, refill float64) *brakeBucket {
	b := m[key]
	if b == nil {
		if len(m) >= brakeMaxEntries {
			rb.evict(m)
		}
		b = &brakeBucket{tokens: capacity, last: rb.now(), cap: capacity, refill: refill}
		m[key] = b
	}
	return b
}

// evict drops entries that are back at full capacity — they carry no state a fresh entry would not
// reconstruct identically, so dropping them is information-free. Only if none are full does it drop
// arbitrary entries, which can only ever FORGIVE a client, never punish one.
func (rb *RejectBrake) evict(m map[string]*brakeBucket) {
	now := rb.now()
	for k, b := range m {
		b.advance(now)
		if b.tokens >= b.cap {
			delete(m, k)
		}
	}
	for k := range m {
		if len(m) < brakeMaxEntries {
			return
		}
		delete(m, k)
	}
}

// Allow reports whether the request should be dispatched. It never consumes a token: a request that is about
// to SUCCEED must not be charged for the refusals that preceded it.
func (rb *RejectBrake) Allow(client, method, path string) bool {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	now := rb.now()
	rt := rb.bucket(rb.routes, client+"|"+method+"|"+path, brakeRouteCap, brakeRouteRefill)
	rt.advance(now)
	if rt.tokens < 1 {
		return false
	}
	cl := rb.bucket(rb.clients, client, brakeClientCap, brakeClientRefill)
	cl.advance(now)
	return cl.tokens >= 1
}

// Record charges the response. A rejection (>= 400) spends one token from both levels. Anything else restores
// this route's budget in full — the client's situation demonstrably changed, and holding a grudge past that
// point would be the brake denying work the server just proved it would do.
//
// A success deliberately does NOT refill the CLIENT level. The console polls a dozen endpoints on a timer, so
// any storming tab is also producing constant successes elsewhere; refilling on those would make the
// rotation-level brake permanently inert — armed, and unable to fire. That is the failure this codebase keeps
// finding: a control that is present, reachable, and cannot reach the state it guards.
func (rb *RejectBrake) Record(client, method, path string, status int) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	now := rb.now()
	rt := rb.bucket(rb.routes, client+"|"+method+"|"+path, brakeRouteCap, brakeRouteRefill)
	rt.advance(now)
	if status < 400 {
		rt.tokens = rt.cap
		return
	}
	cl := rb.bucket(rb.clients, client, brakeClientCap, brakeClientRefill)
	cl.advance(now)
	if rt.tokens >= 1 {
		rt.tokens--
	}
	if cl.tokens >= 1 {
		cl.tokens--
	}
}

// brakeClientKey identifies the caller for braking purposes. A session cookie is preferred and hashed — two
// tabs of one operator share it, which is correct (they share the bug), while two different operators never
// collide. Only when there is no cookie does it fall back to the peer address, which behind a reverse proxy
// is shared: acceptable, because every request that shares that key was ALREADY being rejected, so the brake
// can only change which refusal they receive, never whether they are refused.
//
// It never reads X-Forwarded-For. That header is client-supplied, so honouring it would let any caller mint a
// fresh brake identity per request and opt out of the control entirely.
func brakeClientKey(r *http.Request) string {
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		sum := sha256.Sum256([]byte(c.Value))
		return "s:" + hex.EncodeToString(sum[:8])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "a:" + host
}

// brakeApplies decides which auth classes the brake covers.
//
// ★ THE LOGIN AND STEP-UP ROUTES ARE EXEMPT, AND THAT EXEMPTION IS THE SAFETY ARGUMENT. They are how a caller
// STOPS being rejected. Braking them would mean a client that had exhausted its budget on refused reads could
// not authenticate its way out — the brake would convert a recoverable state into a lockout, which is exactly
// the operator harm it must never cause. Both already carry a dedicated credential-failure limiter
// (ErrLoginRateLimited), which is the correct control for a credential guess.
func brakeApplies(m AuthMethod) bool {
	switch m {
	case AuthOperatorLogin, AuthAdminElevate:
		return false
	default:
		return true
	}
}

// brakeRecorder captures the status code so Record can charge the right thing. It forwards Flush so the SSE
// routes (/v1/sessions/{ref}/stream) keep streaming — a wrapper that silently swallowed Flush would convert
// the live tracer into a page that never updates, and nothing else in the request path would report it.
type brakeRecorder struct {
	http.ResponseWriter
	status int
}

func (b *brakeRecorder) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
	b.ResponseWriter.WriteHeader(code)
}

func (b *brakeRecorder) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.ResponseWriter.Write(p)
}

func (b *brakeRecorder) Flush() {
	if f, ok := b.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// statusOrOK reports the status actually sent. A handler that wrote nothing and set nothing sent 200.
func (b *brakeRecorder) statusOrOK() int {
	if b.status == 0 {
		return http.StatusOK
	}
	return b.status
}
