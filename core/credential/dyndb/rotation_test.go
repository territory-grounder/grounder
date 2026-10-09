package dyndb

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TG-422 slice 2 oracles: renewal cannot outlive the role's max_ttl, so a Manager that only renews goes
// dark there — these pin the ROTATION that replaces it. The recordingBao knobs simulate the two shapes the
// live engine produces near the ceiling: a shrunken grant and a refused renewal.

// A renewal whose grant shrinks to ≤ half the requested increment means the lease is butting against its
// max_ttl: the Manager must mint a FRESH lease, serve it, and revoke the old one. KILLING MUTATION: revert
// step() to renew-only — issues stays 1, Current keeps serving v-user-1, and this fails on all three asserts.
func TestManagerRotatesWhenRenewalGrantShrinks(t *testing.T) {
	bao := &recordingBao{ttl: 3600, renewTTL: 600}
	eng := testEngine(t, bao.doer())
	clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	m := NewManager(eng, "tg_runtime", WithClock(clk.Now))
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	clk.advance(2400 * time.Second) // past the 66% renew point
	m.step(context.Background(), clk.Now())
	if bao.issues != 2 {
		t.Fatalf("issues = %d, want 2 — a max_ttl-capped renewal (600s granted of 3600s asked) must rotate, not limp", bao.issues)
	}
	if bao.revokes != 1 {
		t.Fatalf("revokes = %d, want 1 — the rotated-out lease must be revoked, not left to expire as an orphan", bao.revokes)
	}
	if u, _, ok := m.Current(); !ok || u != "v-user-2" {
		t.Fatalf("Current = (%q,%v), want the FRESH lease v-user-2", u, ok)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if bao.revokes != 2 {
		t.Fatalf("revokes after Close = %d, want 2 — Close must revoke the CURRENT (rotated-in) lease", bao.revokes)
	}
}

// A rotation must fire the onRotate hook EXACTLY ONCE and BEFORE the old lease is revoked. The composition
// root wires pool.Reset there so a connection dialed under the rotated-out lease role — which survives the
// DROP but goes UNPRIVILEGED (verified on the live pg16: current_user "invalid role OID", every table read
// permission-denied, TG-553) — is evicted the instant the lease rotates, not left to fail permission-denied
// until MaxConnLifetime (15m) recycles it. KILLING MUTATION: delete the hook() call in step() — fired stays 0;
// move it AFTER m.eng.Revoke — revokesAtHook becomes 1 and the ordering assert fails.
func TestManagerFiresOnRotateHookBeforeRevoke(t *testing.T) {
	bao := &recordingBao{ttl: 3600, renewTTL: 600}
	eng := testEngine(t, bao.doer())
	clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	m := NewManager(eng, "tg_runtime", WithClock(clk.Now))
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	fired := 0
	revokesAtHook := -1
	m.SetOnRotate(func() {
		fired++
		revokesAtHook = bao.revokes // the revoke must NOT have happened yet when the pool is recycled
	})
	clk.advance(2400 * time.Second) // past the 66% renew point → the max_ttl-capped renewal forces a rotation
	m.step(context.Background(), clk.Now())
	if fired != 1 {
		t.Fatalf("onRotate fired %d times, want exactly 1 — a lease rotation must recycle the pool (TG-553)", fired)
	}
	if revokesAtHook != 0 {
		t.Fatalf("onRotate saw revokes=%d at fire time, want 0 — the pool must be evicted BEFORE the old lease is "+
			"dropped, else a redialed connection can race the DROP (TG-553)", revokesAtHook)
	}
	if bao.issues != 2 || bao.revokes != 1 {
		t.Fatalf("issues=%d revokes=%d after step, want 2/1 — the rotation itself must still mint+revoke", bao.issues, bao.revokes)
	}
	// A renewal that does NOT rotate must not fire the hook — otherwise every tick would pointlessly churn the pool.
	bao2 := &recordingBao{ttl: 3600, renewTTL: 3000} // a healthy renewal (grant > half) — renew, do not rotate
	eng2 := testEngine(t, bao2.doer())
	clk2 := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	m2 := NewManager(eng2, "tg_runtime", WithClock(clk2.Now))
	if err := m2.Start(context.Background()); err != nil {
		t.Fatalf("start m2: %v", err)
	}
	firedOnRenew := 0
	m2.SetOnRotate(func() { firedOnRenew++ })
	clk2.advance(2400 * time.Second)
	m2.step(context.Background(), clk2.Now())
	if firedOnRenew != 0 {
		t.Fatalf("onRotate fired %d times on a plain RENEWAL, want 0 — only a rotation drops the role, so only a "+
			"rotation may recycle the pool (TG-553)", firedOnRenew)
	}
}

// ArmRotationEviction is the SHARED seam both composition roots (cmd/worker + cmd/grounder) use to wire
// pool.Reset to lease rotation — a dropped lease role's live connections go UNPRIVILEGED, not dead (TG-553),
// and BOTH binaries build a dyn: pool, so the fix lives here once. It must (1) arm on a live provider and say
// ARMED, and (2) be BEST-EFFORT: a provider that cannot arm (closed) logs the backstop note and returns —
// never panics, never fails the boot. The rotation mechanism it wires is proven by
// TestManagerFiresOnRotateHookBeforeRevoke; this pins the wrapper's two branches. KILLING MUTATION: make the
// error path panic instead of logging — the closed-provider arm below fails (panic) instead of recording the
// "could not arm … backstops" line.
func TestArmRotationEvictionIsBestEffort(t *testing.T) {
	bao := &recordingBao{ttl: 3600}
	eng := testEngine(t, bao.doer())
	p, err := NewProvider(ProviderConfig{Engine: eng, DSNTemplate: "postgres://postgres:5432/grounder?sslmode=disable"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }

	// (1) live provider: arms and says ARMED.
	ArmRotationEviction(p, "tg_runtime", func() {}, logf)
	if len(logs) != 1 || !strings.Contains(logs[0], "ARMED") {
		t.Fatalf("logs=%v, want exactly one ARMED line on a live provider", logs)
	}

	// (2) best-effort: a CLOSED provider cannot arm — it must log the backstop note and NOT panic / NOT fail boot.
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	logs = nil
	ArmRotationEviction(p, "tg_runtime", func() {}, logf) // must not panic
	if len(logs) != 1 || !strings.Contains(logs[0], "could not arm") || !strings.Contains(logs[0], "backstops") {
		t.Fatalf("logs=%v, want one best-effort 'could not arm … backstops' line — a wiring error must never fail the boot", logs)
	}
}

// A renewal the substrate refuses outright must also rotate — the pre-slice-2 behaviour (keep the old
// expiry, go dark at TTL) is now the LAST resort, taken only when the mint fails too.
func TestManagerRotatesOnRenewalError(t *testing.T) {
	bao := &recordingBao{ttl: 3600, renewFail: true}
	eng := testEngine(t, bao.doer())
	clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	m := NewManager(eng, "tg_runtime", WithClock(clk.Now))
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	clk.advance(2400 * time.Second)
	m.step(context.Background(), clk.Now())
	if bao.issues != 2 || bao.revokes != 1 {
		t.Fatalf("issues=%d revokes=%d, want 2/1 — a refused renewal must mint a fresh lease and revoke the old", bao.issues, bao.revokes)
	}
	if u, _, ok := m.Current(); !ok || u != "v-user-2" {
		t.Fatalf("Current = (%q,%v), want v-user-2", u, ok)
	}
}

// When rotation ALSO fails, a shrunken-but-real renewal grant is kept: it is genuine validity, and the next
// tick retries the rotation with that headroom. Current must serve inside the remnant and fail closed past
// it — never a stale credential. KILLING MUTATION: discard the remnant on mint failure — the in-remnant
// Current below fails; serve past the remnant — the post-remnant assert fails.
func TestManagerKeepsRenewedRemnantWhenRotationFails(t *testing.T) {
	bao := &recordingBao{ttl: 3600, renewTTL: 600, issueFailAfter: 1}
	eng := testEngine(t, bao.doer())
	clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	m := NewManager(eng, "tg_runtime", WithClock(clk.Now))
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	clk.advance(2400 * time.Second)
	m.step(context.Background(), clk.Now()) // renew grants 600s; the rotate mint is refused
	if u, _, ok := m.Current(); !ok || u != "v-user-1" {
		t.Fatalf("Current = (%q,%v), want the ORIGINAL lease still serving inside its renewed remnant", u, ok)
	}
	clk.advance(601 * time.Second) // past the remnant
	if _, _, ok := m.Current(); ok {
		t.Fatal("Current must fail closed once the renewed remnant passes — a stale credential is the one forbidden outcome")
	}
}

// RenewSelf must hit exactly auth/token/renew-self and fail closed on a non-2xx. The policy carve-out this
// call depends on (an exact-path allow beating the auth/* deny) is documented on the method and in the HCL.
func TestEngineRenewSelf(t *testing.T) {
	bao := &recordingBao{ttl: 3600}
	eng := testEngine(t, bao.doer())
	if err := eng.RenewSelf(context.Background()); err != nil {
		t.Fatalf("RenewSelf: %v", err)
	}
	if bao.selfRenews != 1 {
		t.Fatalf("selfRenews = %d, want 1", bao.selfRenews)
	}
	denied := testEngine(t, doerFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResp(403, `{"errors":["permission denied"]}`), nil
	}))
	if err := denied.RenewSelf(context.Background()); err == nil {
		t.Fatal("a refused renew-self must fail closed — the policy carve-out regression this would mask is exactly the auth/* deny eating the exact-path allow")
	}
}

// LookupSelf reads the engine token's keep-alive shape (renewable + period) and fails closed on a non-2xx —
// it is the boot self-check that turns a mis-provisioned (non-periodic) token into a loud signal instead of
// a silent age-out weeks later (TG-545).
func TestEngineLookupSelf(t *testing.T) {
	// A periodic, renewable token — the healthy shape.
	eng := testEngine(t, doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/auth/token/lookup-self") {
			return jsonResp(500, `{"errors":["wrong path"]}`), nil
		}
		return jsonResp(200, `{"data":{"renewable":true,"period":86400,"ttl":3600}}`), nil
	}))
	self, err := eng.LookupSelf(context.Background())
	if err != nil {
		t.Fatalf("LookupSelf: %v", err)
	}
	if !self.Renewable || self.Period != 24*time.Hour || self.TTL != time.Hour {
		t.Fatalf("parsed %+v, want renewable + period=24h + ttl=1h", self)
	}
	// A NON-periodic token (period 0) — the mis-provisioned shape the boot check must flag: renew-self runs
	// but cannot stop it ageing out at max_ttl.
	eng2 := testEngine(t, doerFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"data":{"renewable":true,"period":0,"ttl":600}}`), nil
	}))
	if self2, _ := eng2.LookupSelf(context.Background()); self2.Period != 0 {
		t.Fatalf("a non-periodic token must parse period=0 (so the boot check flags it), got %s", self2.Period)
	}
	// Fail closed on a refusal (lookup-self does not need the renew-self carve-out, but the call still fails
	// closed on any non-2xx).
	denied := testEngine(t, doerFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResp(403, `{"errors":["permission denied"]}`), nil
	}))
	if _, err := denied.LookupSelf(context.Background()); err == nil {
		t.Fatal("a refused lookup-self must fail closed")
	}
}

// Credentials is the per-connection seam ConnectDynamic feeds to pgx's BeforeConnect: it must serve the
// CURRENT lease (v-user-1 here), fail closed after Close, and refuse a malformed role by shape alone.
func TestProviderCredentialsServesCurrentLeaseAndFailsClosed(t *testing.T) {
	bao := &recordingBao{ttl: 3600}
	eng := testEngine(t, bao.doer())
	p, err := NewProvider(ProviderConfig{Engine: eng, DSNTemplate: "postgres://postgres:5432/grounder?sslmode=disable"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	cred := p.Credentials("tg_runtime")
	u, pw, err := cred(context.Background())
	if err != nil || u != "v-user-1" || pw != "pw-1" {
		t.Fatalf("cred = (%q,%q,%v), want the live v-user-1/pw-1", u, pw, err)
	}
	if _, _, err := p.Credentials("../../sys")(context.Background()); err == nil {
		t.Fatal("a malformed role must be refused at the seam, by shape")
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, _, err := cred(context.Background()); err == nil {
		t.Fatal("Credentials must fail closed after Close — a revoked lease must never be served")
	}
}
