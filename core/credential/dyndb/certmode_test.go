package dyndb

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

// TG-565: the engine's own identity must survive an outage of any length. The 2026-09-18 incident: the stack
// was down 57h, the STORED periodic token expired unrenewed, and on restart every mint 403'd — grounder and
// worker refused to boot until a human re-minted the token. Cert mode logs in with the host certificate, so
// a dead token is replaced by logging in again. These oracles pin the login, the re-login on a DEAD token
// (and only a dead one), the one-login-per-death rule under concurrency, the immediate rotation of every lease
// on a token change, and the keep-alive check that now runs at start instead of 6h after boot.

// certBao is a fake OpenBao that models what the incident depended on: tokens that can DIE, and leases that
// die WITH the token that minted them (OpenBao revokes a token's leases when the token goes).
type certBao struct {
	mu         sync.Mutex
	wantRole   string
	loginFail  int  // HTTP status login answers with when non-zero
	emptyToken bool // login answers 200 with no client_token
	logins     int
	loginNames []string
	nextTok    int
	live       map[string]bool   // token → alive
	leaseOwner map[string]string // lease id → minting token
	issues     int
	selfRenews int
	ttl        int // lookup-self ttl (seconds)
	period     int // lookup-self period (seconds)
	forbidRole string
	calls      []string
}

func newCertBao() *certBao {
	return &certBao{wantRole: "tg-dyndb", live: map[string]bool{}, leaseOwner: map[string]string{}, ttl: 86000, period: 86400}
}

// kill expires every token issued so far — and, as OpenBao does, every lease they minted.
func (b *certBao) kill() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for t := range b.live {
		b.live[t] = false
	}
}

func (b *certBao) snapshot() (logins, issues, selfRenews int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.logins, b.issues, b.selfRenews
}

func (b *certBao) doer() doerFunc {
	return func(r *http.Request) (*http.Response, error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.calls = append(b.calls, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPost && r.URL.Path == "/v1/auth/cert/login" {
			if r.Header.Get("X-Vault-Token") != "" {
				return jsonResp(400, `{"errors":["login carries no token"]}`), nil
			}
			var body struct {
				Name string `json:"name"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			b.loginNames = append(b.loginNames, body.Name)
			if b.loginFail != 0 {
				return jsonResp(b.loginFail, `{"errors":["login refused"]}`), nil
			}
			if body.Name != b.wantRole {
				return jsonResp(400, `{"errors":["no such cert role"]}`), nil
			}
			if b.emptyToken {
				return jsonResp(200, `{"auth":{"client_token":""}}`), nil
			}
			b.logins++
			b.nextTok++
			tok := fmt.Sprintf("s.cert-%d", b.nextTok)
			b.live[tok] = true
			return jsonResp(200, `{"auth":{"client_token":"`+tok+`","lease_duration":86400,"renewable":true}}`), nil
		}
		tok := r.Header.Get("X-Vault-Token")
		if !b.live[tok] {
			return jsonResp(403, `{"errors":["permission denied"]}`), nil
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/auth/token/lookup-self"):
			return jsonResp(200, fmt.Sprintf(`{"data":{"renewable":true,"period":%d,"ttl":%d}}`, b.period, b.ttl)), nil
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/auth/token/renew-self"):
			b.selfRenews++
			return jsonResp(200, `{}`), nil
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/creds/"):
			if b.forbidRole != "" && strings.HasSuffix(r.URL.Path, "/"+b.forbidRole) {
				return jsonResp(403, `{"errors":["permission denied"]}`), nil
			}
			b.issues++
			id := "lease-" + itoa(b.issues)
			b.leaseOwner[id] = tok
			return jsonResp(200, `{"lease_id":"`+id+`","lease_duration":3600,"renewable":true,"data":{"username":"v-cert-`+
				itoa(b.issues)+`","password":"pw-`+itoa(b.issues)+`"}}`), nil
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/sys/leases/renew"):
			return jsonResp(200, `{"lease_duration":3600}`), nil
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/sys/leases/revoke"):
			return jsonResp(204, ``), nil
		}
		return jsonResp(404, `{"errors":["no route"]}`), nil
	}
}

func newCertEngine(t *testing.T, d Doer) *Engine {
	t.Helper()
	e, err := New(Config{BaseURL: "https://bao.example:8200", CertRole: "tg-dyndb", CertPath: "/secrets/tg.crt", KeyPath: "/secrets/tg.key", HTTP: d})
	if err != nil {
		t.Fatalf("New (cert mode): %v", err)
	}
	return e
}

// Cert mode logs in (naming the role — an empty name matches EVERY cert role), presents the login token, and
// mints. KILLING MUTATION: drop the "name" from the login body → the fake refuses the role and this fails.
func TestCertModeLogsInAndMints(t *testing.T) {
	bao := newCertBao()
	eng := newCertEngine(t, bao.doer())
	l, err := eng.Issue(context.Background(), "tg_runtime")
	if err != nil {
		t.Fatalf("Issue in cert mode: %v", err)
	}
	if l.Username != "v-cert-1" {
		t.Fatalf("username = %q, want the minted v-cert-1", l.Username)
	}
	logins, issues, _ := bao.snapshot()
	if logins != 1 || issues != 1 {
		t.Fatalf("logins=%d issues=%d, want 1/1", logins, issues)
	}
	if got := bao.loginNames; len(got) != 1 || got[0] != "tg-dyndb" {
		t.Fatalf("login named %v, want exactly [tg-dyndb] — an unnamed login matches every cert role", got)
	}
	if _, err := eng.Issue(context.Background(), "tg_triage"); err != nil {
		t.Fatalf("second Issue: %v", err)
	}
	if logins, _, _ := bao.snapshot(); logins != 1 {
		t.Fatalf("logins=%d after a second mint, want 1 — a live token is reused, not re-minted per call", logins)
	}
}

// THE INCIDENT, in miniature: the token dies (the 57h outage outlived its period). Token mode would 403 for
// ever; cert mode re-logs in once, the mint succeeds, and the token change is reported exactly once.
// KILLING MUTATION: remove the re-login branch in Engine.call → the post-kill Issue 403s and this fails.
func TestCertModeRelogsInOnDeadToken(t *testing.T) {
	bao := newCertBao()
	eng := newCertEngine(t, bao.doer())
	changes := 0
	eng.SetOnTokenChange(func() { changes++ })
	if _, err := eng.Issue(context.Background(), "tg_runtime"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	bao.kill()
	l, err := eng.Issue(context.Background(), "tg_runtime")
	if err != nil {
		t.Fatalf("Issue after the token died must re-login and succeed, got %v", err)
	}
	if l.Username != "v-cert-2" {
		t.Fatalf("username = %q, want v-cert-2 minted under the NEW token", l.Username)
	}
	if logins, _, _ := bao.snapshot(); logins != 2 {
		t.Fatalf("logins = %d, want 2 (boot + one re-login)", logins)
	}
	if changes != 1 || eng.TokenChanges() != 1 {
		t.Fatalf("token change reported %d times (engine counts %d), want exactly 1", changes, eng.TokenChanges())
	}
}

// N callers hitting the same dead token produce ONE re-login, not N (each extra login is a token whose
// leases nobody renews). Run under -race.
func TestCertModeConcurrentDeadTokenRelogsInOnce(t *testing.T) {
	bao := newCertBao()
	eng := newCertEngine(t, bao.doer())
	if _, err := eng.Issue(context.Background(), "tg_runtime"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	bao.kill()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := eng.Issue(context.Background(), "tg_runtime"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Issue after token death: %v", err)
	}
	if logins, _, _ := bao.snapshot(); logins != 2 {
		t.Fatalf("logins = %d, want 2 — concurrent 403s on one dead token must re-login ONCE", logins)
	}
}

// A 403 with a LIVE token is a policy refusal, not a death: no re-login, no token change, and it fails closed.
// KILLING MUTATION: drop the tokenDead() check from Engine.call → a re-login (logins=2) and a spurious token
// change (every lease rotated) on every refused call, and this fails.
func TestCertModePolicyRefusalDoesNotRelogin(t *testing.T) {
	bao := newCertBao()
	bao.forbidRole = "tg_forbidden"
	eng := newCertEngine(t, bao.doer())
	if _, err := eng.Issue(context.Background(), "tg_runtime"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err := eng.Issue(context.Background(), "tg_forbidden")
	if err == nil || !IsForbidden(err) {
		t.Fatalf("a refused role must fail closed with a 403, got %v", err)
	}
	if logins, _, _ := bao.snapshot(); logins != 1 || eng.TokenChanges() != 0 {
		t.Fatalf("logins=%d changes=%d after a policy refusal, want 1/0 — a live token was thrown away", logins, eng.TokenChanges())
	}
}

// A refused login — or one that answers 200 with NO token (the empty-input case) — fails closed: no token is
// presented, nothing is minted.
func TestCertModeFailsClosedWhenLoginRefused(t *testing.T) {
	for name, set := range map[string]func(*certBao){
		"refused":         func(b *certBao) { b.loginFail = 403 },
		"empty token 200": func(b *certBao) { b.emptyToken = true },
	} {
		bao := newCertBao()
		set(bao)
		eng := newCertEngine(t, bao.doer())
		if _, err := eng.Issue(context.Background(), "tg_runtime"); err == nil {
			t.Errorf("%s: the mint must fail closed", name)
		}
		bao.mu.Lock()
		for _, c := range bao.calls {
			if strings.Contains(c, "/creds/") {
				t.Errorf("%s: %q was sent without a token from a successful login — the engine must stop at the login", name, c)
			}
		}
		bao.mu.Unlock()
	}
}

// New refuses a missing or incomplete identity, and cert mode takes precedence over a stored token.
func TestNewEngineIdentityRules(t *testing.T) {
	base := "https://bao.example:8200"
	cases := map[string]Config{
		"no identity":          {BaseURL: base, HTTP: doerFunc(nil)},
		"cert role, no cert":   {BaseURL: base, CertRole: "tg-dyndb", KeyPath: "/k", HTTP: doerFunc(nil)},
		"cert role, no key":    {BaseURL: base, CertRole: "tg-dyndb", CertPath: "/c", HTTP: doerFunc(nil)},
		"cert role, bad chars": {BaseURL: base, CertRole: "../sys", CertPath: "/c", KeyPath: "/k", HTTP: doerFunc(nil)},
		"unreadable key pair":  {BaseURL: base, CertRole: "tg-dyndb", CertPath: "/nonexistent/c", KeyPath: "/nonexistent/k"},
	}
	for name, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New must refuse", name)
		}
	}
	// Both identities set ⇒ cert mode; the stored token is never presented.
	t.Setenv("DYNDB_STORED_TOKEN", "s.stored-must-not-be-used")
	bao := newCertBao()
	eng, err := New(Config{BaseURL: base, TokenRef: config.SecretRef("env:DYNDB_STORED_TOKEN"), CertRole: "tg-dyndb",
		CertPath: "/c", KeyPath: "/k", HTTP: bao.doer()})
	if err != nil {
		t.Fatalf("New with both identities: %v", err)
	}
	if eng.CertRole() != "tg-dyndb" {
		t.Fatalf("CertRole() = %q, want cert mode to win", eng.CertRole())
	}
	if _, err := eng.Issue(context.Background(), "tg_runtime"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if logins, _, _ := bao.snapshot(); logins != 1 {
		t.Fatalf("logins = %d, want 1 — cert mode must log in, not present the stored token", logins)
	}
}

// When the engine's token changes, every lease is rotated NOW (they died with the old token), each pool's
// eviction hook fires, and the keep-alive check is what notices the death. This is the running-process half
// of the incident: OpenBao was unreachable past the token's period, then came back.
// KILLING MUTATION: drop SetOnTokenChange in Register → the leases are never rotated and this fails.
func TestProviderRotatesEveryLeaseWhenTheEngineTokenChanges(t *testing.T) {
	config.RegisterSchemeResolver(Scheme, nil)
	t.Cleanup(func() { config.RegisterSchemeResolver(Scheme, nil) })
	bao := newCertBao()
	eng := newCertEngine(t, bao.doer())
	var logMu sync.Mutex
	var logs []string
	logf := func(f string, a ...any) { logMu.Lock(); logs = append(logs, fmt.Sprintf(f, a...)); logMu.Unlock() }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := Register(true, ProviderConfig{Engine: eng, DSNTemplate: "postgres://h:5432/db", RootCtx: ctx}, logf)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer func() { _ = p.Close(context.Background()) }()
	evicted := make(chan string, 4)
	for _, role := range []string{"tg_runtime", "tg_triage"} {
		r := role
		if err := p.OnRotate(r, func() { evicted <- r }); err != nil {
			t.Fatalf("OnRotate(%s): %v", r, err)
		}
	}
	_, issuesBefore, _ := bao.snapshot()

	bao.kill() // the token and both leases die
	p.tokenTick(ctx, 0, logf)

	deadline := time.After(5 * time.Second)
	got := map[string]bool{}
	for len(got) < 2 {
		select {
		case r := <-evicted:
			got[r] = true
		case <-deadline:
			t.Fatalf("only %v rotated after the engine token changed — a lease minted under the dead token would be served", got)
		}
	}
	if _, issues, _ := bao.snapshot(); issues != issuesBefore+2 {
		t.Fatalf("issues = %d, want %d (one fresh lease per role)", issues, issuesBefore+2)
	}
	for _, role := range []string{"tg_runtime", "tg_triage"} {
		u, _, err := p.Credentials(role)(context.Background())
		if err != nil {
			t.Fatalf("Credentials(%s) after rotation: %v", role, err)
		}
		bao.mu.Lock()
		owner := ""
		for id, tok := range bao.leaseOwner {
			if "v-cert-"+strings.TrimPrefix(id, "lease-") == u {
				owner = tok
			}
		}
		alive := bao.live[owner]
		bao.mu.Unlock()
		if !alive {
			t.Fatalf("%s serves %q, minted under a DEAD token", role, u)
		}
	}
	logMu.Lock()
	defer logMu.Unlock()
	if !strings.Contains(strings.Join(logs, "\n"), "engine token CHANGED") {
		t.Fatalf("the rotation was not logged; logs:\n%s", strings.Join(logs, "\n"))
	}
}

// The keep-alive check runs AT START — the old loop's first renew came 6h after boot, so a stack restarted
// more often than that never renewed its token (the starvation twin of the incident) — and renews only when
// less than 3/4 of the period is left. KILLING MUTATION: wait for the first tick before the first check in
// maintainToken → selfRenews stays 0 here and this fails.
func TestMaintainTokenChecksAtStartAndRenewsWhenDue(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ttl       int
		wantRenew bool
	}{
		{"fresh token: no renew", 86000, false},
		{"ttl under 3/4 of the period: renew now", 17 * 3600, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bao := newCertBao()
			bao.ttl = tc.ttl
			eng := newCertEngine(t, bao.doer())
			p, err := NewProvider(ProviderConfig{Engine: eng, DSNTemplate: "postgres://h:5432/db"})
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { p.maintainToken(ctx, time.Hour, func(string, ...any) {}); close(done) }()
			deadline := time.Now().Add(3 * time.Second)
			for {
				logins, _, renews := bao.snapshot()
				if logins == 1 && (!tc.wantRenew || renews == 1) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("logins=%d renews=%d, want the check to have run at start", logins, renews)
				}
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
			<-done
			if _, _, renews := bao.snapshot(); (renews == 1) != tc.wantRenew {
				t.Fatalf("selfRenews = %d, wantRenew=%v", renews, tc.wantRenew)
			}
		})
	}
}

func TestRenewSelfDue(t *testing.T) {
	for _, tc := range []struct {
		self TokenSelf
		want bool
	}{
		{TokenSelf{Renewable: true, Period: 24 * time.Hour, TTL: 23 * time.Hour}, false},
		{TokenSelf{Renewable: true, Period: 24 * time.Hour, TTL: 17 * time.Hour}, true},
		{TokenSelf{Renewable: false, Period: 24 * time.Hour, TTL: time.Minute}, false},
		{TokenSelf{Renewable: true, Period: 0, TTL: time.Hour}, true},
	} {
		if got := renewSelfDue(tc.self); got != tc.want {
			t.Errorf("renewSelfDue(%+v) = %v, want %v", tc.self, got, tc.want)
		}
	}
}

// The boot line must tell the operator which identity is in use — and call a DEAD stored token dead, not
// "could not verify" (on 2026-09-18 the boot said "could not verify" and then refused to boot).
func TestRegisterBootLineNamesIdentityAndDeadToken(t *testing.T) {
	config.RegisterSchemeResolver(Scheme, nil)
	t.Cleanup(func() { config.RegisterSchemeResolver(Scheme, nil) })
	capture := func() (func(string, ...any), func() string) {
		var mu sync.Mutex
		var b strings.Builder
		return func(f string, a ...any) { mu.Lock(); fmt.Fprintf(&b, f+"\n", a...); mu.Unlock() },
			func() string { mu.Lock(); defer mu.Unlock(); return b.String() }
	}

	logf, read := capture()
	ctx, cancel := context.WithCancel(context.Background())
	p, err := Register(true, ProviderConfig{Engine: newCertEngine(t, newCertBao().doer()), DSNTemplate: "postgres://h:5432/db", RootCtx: ctx}, logf)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	cancel()
	_ = p.Close(context.Background())
	if !strings.Contains(read(), `engine identity = OpenBao cert role "tg-dyndb"`) {
		t.Fatalf("cert-mode boot line missing; got:\n%s", read())
	}

	logf, read = capture()
	dead := testEngine(t, doerFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResp(403, `{"errors":["permission denied"]}`), nil
	}))
	ctx2, cancel2 := context.WithCancel(context.Background())
	p2, err := Register(true, ProviderConfig{Engine: dead, DSNTemplate: "postgres://h:5432/db", RootCtx: ctx2}, logf)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	cancel2()
	_ = p2.Close(context.Background())
	if out := read(); !strings.Contains(out, "engine token is DEAD") || !strings.Contains(out, "TG_DYNDB_CERT_ROLE") {
		t.Fatalf("a dead stored token must be called DEAD and name the cure; got:\n%s", out)
	}

	// Cert mode: a REFUSED login (4xx — wrong role, CN not allowed, expired cert) is a misconfiguration that
	// waiting never fixes, and must not be told "re-logs in by itself"; an OpenBao failure (5xx) is.
	for _, tc := range []struct {
		status          int
		want, mustNotBe string
	}{
		{400, "MISCONFIGURATION", "re-logs in by itself"},
		{503, "re-logs in by itself", "MISCONFIGURATION"},
	} {
		logf, read = capture()
		bao := newCertBao()
		bao.loginFail = tc.status
		ctx3, cancel3 := context.WithCancel(context.Background())
		p3, err := Register(true, ProviderConfig{Engine: newCertEngine(t, bao.doer()), DSNTemplate: "postgres://h:5432/db", RootCtx: ctx3}, logf)
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		cancel3()
		_ = p3.Close(context.Background())
		if out := read(); !strings.Contains(out, tc.want) || strings.Contains(out, tc.mustNotBe) {
			t.Fatalf("login status %d: boot line must say %q and not %q; got:\n%s", tc.status, tc.want, tc.mustNotBe, out)
		}
	}
}

// Token changes arriving while a rotation round is running COALESCE into one more round — an OpenBao flapping
// between answering and not must not fan out one full rotation per flap. KILLING MUTATION: drop the
// rotRunning early-return in requestRotateAll → 5 rounds here, and this fails.
func TestProviderCoalescesTokenChangeRotations(t *testing.T) {
	bao := newCertBao()
	inner := bao.doer()
	gate := make(chan struct{})
	var gated sync.Mutex
	gateOn := false
	d := doerFunc(func(r *http.Request) (*http.Response, error) {
		gated.Lock()
		on := gateOn
		gated.Unlock()
		if on && strings.Contains(r.URL.Path, "/creds/") {
			<-gate
		}
		return inner(r)
	})
	eng := newCertEngine(t, d)
	p, err := NewProvider(ProviderConfig{Engine: eng, DSNTemplate: "postgres://h:5432/db"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	defer func() { _ = p.Close(context.Background()) }()
	var mu sync.Mutex
	rounds := 0
	p.logf = func(f string, _ ...any) {
		if strings.Contains(f, "engine token CHANGED") {
			mu.Lock()
			rounds++
			mu.Unlock()
		}
	}
	if _, _, err := p.Credentials("tg_runtime")(context.Background()); err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	gated.Lock()
	gateOn = true
	gated.Unlock()
	for i := 0; i < 5; i++ {
		p.requestRotateAll() // the first round blocks inside its mint; the other four must coalesce
	}
	close(gate)
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.rotMu.Lock()
		running := p.rotRunning
		p.rotMu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rotation rounds never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if rounds != 2 {
		t.Fatalf("rotation rounds = %d for 5 changes during one round, want 2 (the running one + ONE coalesced)", rounds)
	}
}

// End to end over REAL mTLS: the engine loads the key pair and presents it on the login AND on renew-self —
// OpenBao binds a cert-auth token to its certificate (disable_binding=false), so a renew without the cert is
// refused. KILLING MUTATION: load the key pair only when logging in (or not at all) → the server sees no
// client certificate on renew-self and this fails.
func TestCertModePresentsTheClientCertOnEveryCall(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeDynDBSelfSignedPair(t, dir)
	var mu sync.Mutex
	sawCert := map[string]bool{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sawCert[r.URL.Path] = r.TLS != nil && len(r.TLS.PeerCertificates) > 0
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/auth/cert/login":
			_, _ = io.WriteString(w, `{"auth":{"client_token":"s.tls-1"}}`)
		case "/v1/auth/token/renew-self":
			if r.Header.Get("X-Vault-Token") != "s.tls-1" {
				w.WriteHeader(403)
				return
			}
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(404)
		}
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	srv.StartTLS()
	defer srv.Close()
	caPath := filepath.Join(dir, "server-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	eng, err := New(Config{BaseURL: srv.URL, CertRole: "tg-dyndb", CertPath: certPath, KeyPath: keyPath, CACert: caPath})
	if err != nil {
		t.Fatalf("New over mTLS: %v", err)
	}
	if err := eng.RenewSelf(context.Background()); err != nil {
		t.Fatalf("RenewSelf over mTLS: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/v1/auth/cert/login", "/v1/auth/token/renew-self"} {
		if !sawCert[path] {
			t.Errorf("%s was reached without the client certificate (saw %v)", path, sawCert)
		}
	}
}

func writeDynDBSelfSignedPair(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "dyndb-test-host"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath = filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}
