package dyndb

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

// --- test doubles -------------------------------------------------------------------------------------

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

// recordingBao is a fake OpenBao that mints DISTINCT creds per Issue, echoes a fixed TTL on Renew, and 204s
// on Revoke. It records the (method, path) of every call so the oracle can assert the right endpoints ran.
// The slice-2 knobs (renewTTL/renewFail/issueFailAfter/selfRenews) default to the original behaviour.
type recordingBao struct {
	mu         sync.Mutex
	issues     int
	renews     int
	revokes    int
	selfRenews int
	calls      []string
	ttl        int // seconds returned by Issue + Renew
	// slice-2 knobs:
	renewTTL       int  // when >0, Renew grants THIS many seconds instead of ttl (a max_ttl-capped shrink)
	renewFail      bool // when true, Renew returns 503 (substrate says no)
	issueFailAfter int  // when >0, Issue returns 503 once `issues` reaches this count (mint N, then refuse)
}

func (b *recordingBao) doer() doerFunc {
	return func(r *http.Request) (*http.Response, error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.calls = append(b.calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/creds/"):
			if b.issueFailAfter > 0 && b.issues >= b.issueFailAfter {
				return jsonResp(503, `{"errors":["mint refused"]}`), nil
			}
			b.issues++
			// distinct username+password per issue — proves minting, not caching.
			body := `{"lease_id":"lease-` + itoa(b.issues) + `","lease_duration":` + itoa(b.ttl) +
				`,"renewable":true,"data":{"username":"v-user-` + itoa(b.issues) + `","password":"pw-` + itoa(b.issues) + `"}}`
			return jsonResp(200, body), nil
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/sys/leases/renew"):
			b.renews++
			if b.renewFail {
				return jsonResp(503, `{"errors":["renew refused"]}`), nil
			}
			granted := b.ttl
			if b.renewTTL > 0 {
				granted = b.renewTTL
			}
			return jsonResp(200, `{"lease_duration":`+itoa(granted)+`}`), nil
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/sys/leases/revoke"):
			b.revokes++
			return jsonResp(204, ``), nil
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/auth/token/renew-self"):
			b.selfRenews++
			return jsonResp(200, `{}`), nil
		default:
			return jsonResp(404, `{"errors":["no route"]}`), nil
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func testEngine(t *testing.T, d Doer) *Engine {
	t.Helper()
	t.Setenv("DYNDB_TEST_TOKEN", "test-root-token")
	e, err := New(Config{BaseURL: "https://bao.example:8200", TokenRef: config.SecretRef("env:DYNDB_TEST_TOKEN"), HTTP: d})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// --- Engine oracle ------------------------------------------------------------------------------------

// Issue must mint a DISTINCT credential each call. KILLING MUTATION: a resolver that cached/returned a static
// credential would return the same username twice and fail here.
func TestEngineIssueMintsDistinct(t *testing.T) {
	bao := &recordingBao{ttl: 3600}
	eng := testEngine(t, bao.doer())
	l1, err := eng.Issue(context.Background(), "tg_runtime")
	if err != nil {
		t.Fatalf("issue 1: %v", err)
	}
	l2, err := eng.Issue(context.Background(), "tg_runtime")
	if err != nil {
		t.Fatalf("issue 2: %v", err)
	}
	if l1.Username == l2.Username || l1.Password == l2.Password {
		t.Fatalf("two issues returned the same credential (%q/%q vs %q/%q) — not minting", l1.Username, l1.Password, l2.Username, l2.Password)
	}
	if l1.Duration != 3600*time.Second {
		t.Fatalf("lease duration = %v, want 1h", l1.Duration)
	}
	if got := bao.calls[0]; got != "GET /v1/database/creds/tg_runtime" {
		t.Fatalf("issue hit %q, want GET /v1/database/creds/tg_runtime", got)
	}
}

func TestEngineFailsClosed(t *testing.T) {
	t.Run("non-2xx is an error, body not echoed", func(t *testing.T) {
		eng := testEngine(t, doerFunc(func(*http.Request) (*http.Response, error) {
			return jsonResp(403, `{"errors":["permission denied: secret-value-leaked"]}`), nil
		}))
		_, err := eng.Issue(context.Background(), "tg_runtime")
		if err == nil {
			t.Fatal("expected fail-closed error on 403")
		}
		if strings.Contains(err.Error(), "secret-value-leaked") || strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("error echoed the response body (INV-13 leak): %q", err.Error())
		}
		if !strings.Contains(err.Error(), "403") {
			t.Fatalf("error should name the status, got %q", err.Error())
		}
	})
	t.Run("empty credential is an error", func(t *testing.T) {
		eng := testEngine(t, doerFunc(func(*http.Request) (*http.Response, error) {
			return jsonResp(200, `{"lease_id":"x","lease_duration":3600,"data":{"username":"","password":""}}`), nil
		}))
		if _, err := eng.Issue(context.Background(), "tg_runtime"); err == nil {
			t.Fatal("an empty credential must fail closed, not resolve to an empty value")
		}
	})
	t.Run("empty role is rejected", func(t *testing.T) {
		eng := testEngine(t, (&recordingBao{ttl: 3600}).doer())
		if _, err := eng.Issue(context.Background(), "  "); err == nil {
			t.Fatal("empty role must error")
		}
	})
}

// A malformed role must be rejected in CODE before it is interpolated into the OpenBao request path — not
// left to the server ACL. No HTTP request may be made for a bad role. KILLING MUTATION: drop the validRole
// check in Issue → a `/`-bearing role reaches the doer and bao.issues>0.
func TestEngineRejectsBadRole(t *testing.T) {
	for _, bad := range []string{"tg/evil", "../creds/root", "tg runtime", "tg_runtime\n", "role;drop"} {
		bao := &recordingBao{ttl: 3600}
		eng := testEngine(t, bao.doer())
		_, err := eng.Issue(context.Background(), bad)
		if err == nil {
			t.Errorf("role %q was accepted — it must be rejected before the path is built", bad)
		}
		if bao.issues != 0 {
			t.Errorf("role %q reached the OpenBao endpoint (issues=%d) — a bad role must never be sent", bad, bao.issues)
		}
		if err != nil && strings.Contains(err.Error(), bad) && strings.ContainsAny(bad, "/;\n ") {
			t.Errorf("error echoed the raw role %q — a misconfigured dyn:<secret> would leak (INV-13)", bad)
		}
	}
}

// dyn: MUST be classified as a backend reference scheme, or the secret-policy gate would reject the armed
// `TG_DB_DSN=dyn:<role>` migration as plaintext — punishing the exact hardening it should reward.
func TestDynIsABackendReferenceScheme(t *testing.T) {
	if !config.IsBackendScheme("dyn:tg_runtime") {
		t.Error("config.IsBackendScheme(\"dyn:...\") is false — the secret-policy gate would flag an armed dyn: DSN as plaintext")
	}
	if !config.HasReferenceScheme("dyn:tg_runtime") {
		t.Error("config.HasReferenceScheme(\"dyn:...\") is false — the env-shape gate would flag an armed dyn: DSN as a raw credential")
	}
}

func TestEngineRenewRevoke(t *testing.T) {
	bao := &recordingBao{ttl: 1800}
	eng := testEngine(t, bao.doer())
	ttl, err := eng.Renew(context.Background(), "lease-1", time.Hour)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if ttl != 1800*time.Second {
		t.Fatalf("renew ttl = %v, want 30m", ttl)
	}
	if err := eng.Revoke(context.Background(), "lease-1"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if bao.renews != 1 || bao.revokes != 1 {
		t.Fatalf("renews=%d revokes=%d, want 1/1 (endpoints: %v)", bao.renews, bao.revokes, bao.calls)
	}
}

// --- Manager lifecycle oracle -------------------------------------------------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

// The lifecycle the ticket pins: Start issues, the lease RENEWS before its TTL, Current serves the live cred,
// and Close REVOKES. KILLING MUTATIONS: break renewDue → renews==0; drop Revoke from Close → revokes==0.
func TestManagerRenewsBeforeTTLAndRevokesOnClose(t *testing.T) {
	bao := &recordingBao{ttl: 3600}
	eng := testEngine(t, bao.doer())
	clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	m := NewManager(eng, "tg_runtime", WithClock(clk.Now))

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if u, _, ok := m.Current(); !ok || u != "v-user-1" {
		t.Fatalf("current after start = (%q,%v), want a live v-user-1", u, ok)
	}

	// Not yet due at 50% elapsed (remaining 1800s > margin 0.34*3600≈1224s).
	clk.advance(1800 * time.Second)
	m.step(context.Background(), clk.Now())
	if bao.renews != 0 {
		t.Fatalf("renewed too early (%d) — at 50%% elapsed there is still a third of the lease left", bao.renews)
	}

	// Past the 66% renew point but BEFORE the TTL (2400s elapsed of 3600s): must renew now.
	clk.advance(600 * time.Second)
	if !clk.Now().Before(time.Unix(1_800_000_000, 0).Add(3600 * time.Second)) {
		t.Fatal("test setup: the renew must happen before the original TTL boundary")
	}
	m.step(context.Background(), clk.Now())
	if bao.renews != 1 {
		t.Fatalf("did not renew before TTL (renews=%d)", bao.renews)
	}

	if _, _, ok := m.Current(); !ok {
		t.Fatal("credential should still be live right after a successful renew")
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if bao.revokes != 1 {
		t.Fatalf("Close did not revoke the lease (revokes=%d)", bao.revokes)
	}
	if _, _, ok := m.Current(); ok {
		t.Fatal("Current must fail closed after Close")
	}
}

// A lease whose renewal stops succeeding must go DARK, never stale: Current fails closed once the TTL passes.
func TestManagerCurrentFailsClosedOnExpiry(t *testing.T) {
	bao := &recordingBao{ttl: 3600}
	eng := testEngine(t, bao.doer())
	clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	m := NewManager(eng, "tg_runtime", WithClock(clk.Now))
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	clk.advance(3601 * time.Second) // past the TTL, no renew happened
	if _, _, ok := m.Current(); ok {
		t.Fatal("Current must fail closed once the held lease has expired without renewal")
	}
}

// --- Provider + scheme oracle -------------------------------------------------------------------------

func TestProviderResolveBuildsDSN(t *testing.T) {
	bao := &recordingBao{ttl: 3600}
	eng := testEngine(t, bao.doer())
	p, err := NewProvider(ProviderConfig{
		Engine:      eng,
		DSNTemplate: "postgres://db.internal:5432/grounder?sslmode=verify-full",
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	defer func() { _ = p.Close(context.Background()) }()

	dsn, err := p.Resolve("dyn:tg_runtime")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The leased userinfo must be injected; the template's host/db/params preserved.
	for _, want := range []string{"v-user-1", "pw-1", "@db.internal:5432/grounder", "sslmode=verify-full"} {
		if !strings.Contains(dsn, want) {
			t.Fatalf("resolved DSN %q missing %q", dsn, want)
		}
	}
}

func TestProviderFailsClosed(t *testing.T) {
	t.Run("template with userinfo is rejected", func(t *testing.T) {
		eng := testEngine(t, (&recordingBao{ttl: 3600}).doer())
		if _, err := NewProvider(ProviderConfig{Engine: eng, DSNTemplate: "postgres://u:p@h:5432/db"}); err == nil {
			t.Fatal("a template embedding static userinfo defeats dyn: and must be rejected")
		}
	})
	t.Run("ref without a role fails closed", func(t *testing.T) {
		eng := testEngine(t, (&recordingBao{ttl: 3600}).doer())
		p, _ := NewProvider(ProviderConfig{Engine: eng, DSNTemplate: "postgres://h:5432/db"})
		if _, err := p.Resolve("dyn:"); err == nil {
			t.Fatal("dyn: with no role must fail closed")
		}
	})
	t.Run("engine that will not mint fails closed", func(t *testing.T) {
		eng := testEngine(t, doerFunc(func(*http.Request) (*http.Response, error) {
			return jsonResp(500, `{}`), nil
		}))
		p, _ := NewProvider(ProviderConfig{Engine: eng, DSNTemplate: "postgres://h:5432/db"})
		if _, err := p.Resolve("dyn:tg_runtime"); err == nil {
			t.Fatal("an engine returning 500 must make Resolve fail closed")
		}
	})
}

// Register OFF must be a true no-op: the dyn: scheme stays unregistered and any dyn: ref fails closed in
// config.SecretRef.Resolve — the guarantee that merging this dormant, un-armed slice changes nothing.
func TestRegisterOffLeavesSchemeUnregistered(t *testing.T) {
	// ensure a clean slate regardless of test order
	config.RegisterSchemeResolver(Scheme, nil)
	t.Cleanup(func() { config.RegisterSchemeResolver(Scheme, nil) })

	p, err := Register(false, ProviderConfig{}, nil)
	if err != nil || p != nil {
		t.Fatalf("Register(false) = (%v,%v), want (nil,nil)", p, err)
	}
	if _, err := config.SecretRef("dyn:tg_runtime").Resolve(); err == nil {
		t.Fatal("with dyn: unregistered, a dyn: reference MUST fail closed")
	}
}

func TestRegisterOnWiresScheme(t *testing.T) {
	config.RegisterSchemeResolver(Scheme, nil)
	t.Cleanup(func() { config.RegisterSchemeResolver(Scheme, nil) })

	eng := testEngine(t, (&recordingBao{ttl: 3600}).doer())
	p, err := Register(true, ProviderConfig{Engine: eng, DSNTemplate: "postgres://h:5432/db"}, nil)
	if err != nil {
		t.Fatalf("Register(true): %v", err)
	}
	defer func() { _ = p.Close(context.Background()) }()
	v, err := config.SecretRef("dyn:tg_runtime").Resolve()
	if err != nil {
		t.Fatalf("dyn: resolve through config after Register(true): %v", err)
	}
	if !strings.Contains(v, "v-user-1") {
		t.Fatalf("resolved value %q is not a leased DSN", v)
	}
}
