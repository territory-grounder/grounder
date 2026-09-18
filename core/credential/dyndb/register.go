package dyndb

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

// Scheme is the SecretRef scheme this package serves. A reference `dyn:<role>` resolves to a live Postgres
// DSN whose userinfo is a fresh OpenBao database-engine lease for <role>.
const Scheme = "dyn"

// Provider serves the `dyn:` scheme. It owns one Manager per distinct role (created on first Resolve), keeps
// each lease renewed via a background goroutine, and revokes every lease at Close. It is wired into
// core/config by Register and is the object the composition root defers Close on at shutdown.
type Provider struct {
	eng     *Engine
	tmpl    *url.URL // parsed base DSN (scheme://host:port/db?params); userinfo is filled per-lease
	tick    time.Duration
	rootCtx context.Context
	logf    func(string, ...any)

	mu       sync.Mutex
	managers map[string]*Manager
	closed   bool

	rotMu      sync.Mutex // coalesces token-change rotations (requestRotateAll)
	rotRunning bool
	rotPending bool
}

// Config for a Provider. DSNTemplate is a Postgres DSN with the CONNECTION coordinates but NO userinfo —
// e.g. "postgres://postgres:5432/grounder?sslmode=verify-full". The role's leased username/password are
// injected into the userinfo at resolve time. Tick is how often each Manager evaluates renewal (default 1m).
type ProviderConfig struct {
	Engine      *Engine
	DSNTemplate string
	Tick        time.Duration
	// RootCtx bounds every Manager's renewal goroutine and Issue/Renew calls; cancel it (or call Close) at
	// shutdown. Defaults to context.Background when nil.
	RootCtx context.Context
}

// NewProvider builds a Provider. Fails closed on a nil engine or a DSN template that is absent, unparseable,
// or already carries userinfo (a template must not embed a static credential — that is the thing dyn: removes).
func NewProvider(cfg ProviderConfig) (*Provider, error) {
	if cfg.Engine == nil {
		return nil, errors.New("dyndb: provider requires an engine")
	}
	if strings.TrimSpace(cfg.DSNTemplate) == "" {
		return nil, errors.New("dyndb: provider requires a DSN template")
	}
	u, err := url.Parse(strings.TrimSpace(cfg.DSNTemplate))
	if err != nil {
		return nil, fmt.Errorf("dyndb: DSN template is not a valid URL: %w", err)
	}
	if u.User != nil {
		return nil, errors.New("dyndb: DSN template must not carry userinfo — dyn: fills the credential per lease")
	}
	if u.Host == "" {
		return nil, errors.New("dyndb: DSN template has no host")
	}
	tick := cfg.Tick
	if tick <= 0 {
		tick = time.Minute
	}
	rc := cfg.RootCtx
	if rc == nil {
		rc = context.Background()
	}
	return &Provider{
		eng:      cfg.Engine,
		tmpl:     u,
		tick:     tick,
		rootCtx:  rc,
		logf:     func(string, ...any) {},
		managers: map[string]*Manager{},
	}, nil
}

// Resolve is the SecretRef scheme resolver (config.RegisterSchemeResolver contract). It accepts the full
// reference `dyn:<role>`, returns a Postgres DSN carrying a live lease for <role>, and fails closed on an
// unknown/empty role, a closed provider, an engine that will not mint, or an expired lease.
func (p *Provider) Resolve(ref string) (string, error) {
	role := strings.TrimSpace(strings.TrimPrefix(ref, Scheme+":"))
	if role == "" || role == strings.TrimSpace(ref) {
		// Empty role, or the ref lacked the dyn: prefix entirely. Name neither the ref nor a value (INV-13).
		return "", errors.New("dyndb: a dyn: reference must name a database role (dyn:<role>)")
	}
	if !validRole.MatchString(role) {
		// Reject a malformed role at the scheme boundary (defence-in-depth with Engine.Issue). Shape only,
		// never the text — a misconfigured dyn:<secret> must not leak into an error or log (INV-13).
		return "", errors.New("dyndb: the database role in a dyn: reference has invalid characters (allowed: A-Za-z0-9_-)")
	}
	m, err := p.manager(role)
	if err != nil {
		return "", err
	}
	user, pass, ok := m.Current()
	if !ok {
		return "", fmt.Errorf("dyndb: no live credential for role %q (fail closed)", role)
	}
	// Build the DSN by injecting the leased userinfo into the template. url.UserPassword escapes both, so a
	// password with URL-significant bytes cannot corrupt the DSN.
	out := *p.tmpl
	out.User = url.UserPassword(user, pass)
	return out.String(), nil
}

// manager returns the role's Manager, creating and starting it (plus its renewal goroutine) on first use.
func (p *Provider) manager(role string) (*Manager, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("dyndb: provider is closed (fail closed)")
	}
	if m, ok := p.managers[role]; ok {
		p.mu.Unlock()
		return m, nil
	}
	p.mu.Unlock()

	m := NewManager(p.eng, role)
	if err := m.Start(p.rootCtx); err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = m.Close(p.rootCtx)
		return nil, errors.New("dyndb: provider is closed (fail closed)")
	}
	// Re-check for a racing creator; keep the first winner and revoke the loser.
	if existing, ok := p.managers[role]; ok {
		p.mu.Unlock()
		_ = m.Close(p.rootCtx)
		return existing, nil
	}
	p.managers[role] = m
	p.mu.Unlock()
	go m.Maintain(p.rootCtx, p.tick)
	return m, nil
}

// OnRotate wires a callback that fires each time the role's lease ROTATES — a fresh lease replaces the old
// one, which is then revoked (its Postgres role DROPped). The composition root passes pool.Reset here so the
// pool's connections, dialed under the OLD lease, are evicted the instant it rotates: a dropped lease role's
// live session is not terminated but goes UNPRIVILEGED (verified on pg16 — current_user reads "invalid role
// OID", every table read is permission-denied), so a connection that outlives its lease must be recycled,
// never trusted to keep working (TG-553). It creates and starts the role's Manager on first use exactly like
// Resolve/Credentials, so a caller need not have resolved the role first; per role, the last writer wins.
func (p *Provider) OnRotate(role string, cb func()) error {
	m, err := p.manager(role)
	if err != nil {
		return err
	}
	m.SetOnRotate(cb)
	return nil
}

// ArmRotationEviction wires reset to fire each time role's lease ROTATES, so a pgx pool built over this
// provider evicts the connections it dialed under the rotated-out lease the instant that lease rotates —
// instead of failing permission-denied until MaxConnLifetime (15m) recycles them. It is the shared seam for
// EVERY composition root that builds a dyn: pool (the worker AND the grounder both do): a dropped lease role's
// live pooled session is not killed but goes UNPRIVILEGED (TG-553), and both binaries were exposed to it, so
// the fix lives here once rather than being re-derived per main(). reset is the pool's Reset method; logf is
// the caller's logger (log.Printf). Best-effort: a wiring error only forfeits the PROACTIVE eviction —
// MaxConnLifetime still backstops it — so it NEVER fails the boot.
func ArmRotationEviction(p *Provider, role string, reset func(), logf func(string, ...any)) {
	if err := p.OnRotate(role, reset); err != nil {
		logf("dyndb: could not arm rotation-eviction for role %q (MaxConnLifetime still backstops): %v", role, err)
		return
	}
	logf("dyndb: rotation-eviction ARMED for role %q — pool recycles on each lease rotation, no connection outlives its lease (TG-553)", role)
}

// Credentials returns a per-connection credential source for one role — the seam db.ConnectDynamic feeds to
// pgx's BeforeConnect (TG-422 slice 2). Each call returns the CURRENT leased username/password (creating and
// starting the role's Manager on first use, exactly like Resolve), so a pool dials every new connection with
// a live lease even after rotation has replaced the one it booted with. Fails closed like Resolve: a closed
// provider, an engine that will not mint, or an expired lease is an error, never a stale value.
func (p *Provider) Credentials(role string) func(context.Context) (string, string, error) {
	return func(context.Context) (string, string, error) {
		if !validRole.MatchString(role) {
			// Shape only, never the text (INV-13) — same boundary rule as Resolve.
			return "", "", errors.New("dyndb: the database role has invalid characters (allowed: A-Za-z0-9_-)")
		}
		m, err := p.manager(role)
		if err != nil {
			return "", "", err
		}
		user, pass, ok := m.Current()
		if !ok {
			return "", "", fmt.Errorf("dyndb: no live credential for role %q (fail closed)", role)
		}
		return user, pass, nil
	}
}

// renewFailureEscalateAfter is how many CONSECUTIVE token-check failures turn the per-tick "will retry" note
// into a loud escalation: a single miss is transient, but a sustained run means the token is dying and the
// plane is heading for a fail-closed DB outage (TG-545) that used to surface only as "failed mints" hours
// later with no proximate cause. While it persists the escalation repeats every escalateEvery failures.
const (
	renewFailureEscalateAfter = 3
	escalateEvery             = 60
)

// TokenCheckEvery is how often the engine's OWN token is checked (lookup-self) and, when due, renewed
// (TG-565). It used to be a bare 6h renew-self ticker whose FIRST tick came 6h after boot — so a stack
// restarted more often than that never renewed at all — and a token that died meanwhile went unnoticed until
// a lease's renew point failed. A one-minute lookup is cheap, and in cert mode it is also what notices a dead
// token and re-logs in within a minute of OpenBao answering again.
const TokenCheckEvery = time.Minute

// renewSelfDue reports whether the token should be renewed now: when less than three quarters of its period
// is left (≈ every 6h for a 24h period). A non-periodic token has no period to measure against; it is renewed
// against a 24h yardstick and the boot check has already called it out.
func renewSelfDue(self TokenSelf) bool {
	if !self.Renewable {
		return false
	}
	ref := self.Period
	if ref <= 0 {
		ref = 24 * time.Hour
	}
	return self.TTL < ref*3/4
}

// maintainToken keeps the engine's OWN bao token alive until ctx is cancelled: a check runs AT START and then
// every `every`, renewing (renew-self) when due. In cert mode the check's lookup re-logs in on a dead token
// (Engine.call), and the token-change hook rotates every lease. A single failed check is logged and retried
// next tick; PERSISTENT failure is escalated loudly (TG-545).
func (p *Provider) maintainToken(ctx context.Context, every time.Duration, logf func(string, ...any)) {
	if every <= 0 {
		every = TokenCheckEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	consecutiveFailures := 0
	for {
		consecutiveFailures = p.tokenTick(ctx, consecutiveFailures, logf)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// tokenTick is one keep-alive check; it returns the updated consecutive-failure count.
func (p *Provider) tokenTick(ctx context.Context, consecutiveFailures int, logf func(string, ...any)) int {
	self, err := p.eng.LookupSelf(ctx)
	if err == nil && renewSelfDue(self) {
		err = p.eng.RenewSelf(ctx)
	}
	if ctx.Err() != nil {
		return consecutiveFailures
	}
	if err != nil {
		consecutiveFailures++
		switch {
		case consecutiveFailures == renewFailureEscalateAfter ||
			(consecutiveFailures > renewFailureEscalateAfter && consecutiveFailures%escalateEvery == 0):
			logf("dyndb: ALERT the engine token check has FAILED %d consecutive times (%v) — the token is not being kept alive; when it ages out, every dyn: mint fails closed and the plane loses its DB (the TG-545 outage). %s", consecutiveFailures, err, p.cureHint(err))
		case consecutiveFailures < renewFailureEscalateAfter:
			logf("dyndb: engine token check failed (will retry, %d consecutive): %v", consecutiveFailures, err)
		}
		return consecutiveFailures
	}
	if consecutiveFailures >= renewFailureEscalateAfter {
		logf("dyndb: engine token check RECOVERED after %d consecutive failures", consecutiveFailures)
	}
	return 0
}

// cureHint names the operator action for a token that cannot be kept alive, per identity mode — and, in cert
// mode, per failure: a REFUSED login (4xx) is a misconfiguration that never heals by waiting, an outage is.
func (p *Provider) cureHint(err error) string {
	if r := p.eng.CertRole(); r != "" {
		if IsLoginRefused(err) {
			return fmt.Sprintf("Cert mode: OpenBao REFUSED the login as role %q — a MISCONFIGURATION that will NOT heal by retrying: check the role exists (auth/cert/certs/%s), its allowed_common_names match the host certificate's CN, and the certificate (TG_OPENBAO_CERT) is unexpired and issued by the role's CA.", r, r)
		}
		return fmt.Sprintf("Cert mode (role %q) re-logs in by itself once OpenBao answers — check OpenBao is reachable and unsealed.", r)
	}
	return "Token mode: nothing can re-mint the stored token — set TG_DYNDB_CERT_ROLE (the cert identity survives any outage, TG-565) or re-mint periodic: bao token create -policy=tg-dyndb-postgres -period=24h"
}

// requestRotateAll is the token-change hook: it runs rotateAll in the background, COALESCING bursts — a change
// that lands while a round is running schedules exactly one more round instead of stacking goroutines (an
// OpenBao flapping between answering and not would otherwise fan out one full rotation per flap).
func (p *Provider) requestRotateAll() {
	p.rotMu.Lock()
	if p.rotRunning {
		p.rotPending = true
		p.rotMu.Unlock()
		return
	}
	p.rotRunning = true
	p.rotMu.Unlock()
	go func() {
		for {
			p.rotateAll(p.rootCtx)
			p.rotMu.Lock()
			if !p.rotPending {
				p.rotRunning = false
				p.rotMu.Unlock()
				return
			}
			p.rotPending = false
			p.rotMu.Unlock()
		}
	}()
}

// rotateAll rotates every held lease now (see Manager.Rotate): the engine's token changed, and OpenBao
// revoked the old token's leases with it.
func (p *Provider) rotateAll(ctx context.Context) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	roles := make([]string, 0, len(p.managers))
	managers := make([]*Manager, 0, len(p.managers))
	for r, m := range p.managers {
		roles = append(roles, r)
		managers = append(managers, m)
	}
	p.mu.Unlock()
	p.logf("dyndb: engine token CHANGED (change #%d) — rotating %d lease(s) %v now: leases minted under the old token died with it (TG-565)", p.eng.TokenChanges(), len(managers), roles)
	for i, m := range managers {
		if err := m.Rotate(ctx); err != nil {
			p.logf("dyndb: rotation after token change FAILED for role %q (the Manager retries at its renew point): %v", roles[i], err)
		}
	}
}

// Close revokes every held lease. Call it once at shutdown (defer it in the composition root). Idempotent.
func (p *Provider) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	managers := make([]*Manager, 0, len(p.managers))
	for _, m := range p.managers {
		managers = append(managers, m)
	}
	p.managers = map[string]*Manager{}
	p.mu.Unlock()
	var firstErr error
	for _, m := range managers {
		if err := m.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Register wires the `dyn:` scheme into core/config when enabled, mirroring the fail-closed delivery.go
// contract. enabled=false (the default — TG_DYNDB_ADDR unset) is a logged no-op: the dyn: scheme stays
// UNREGISTERED, so any dyn: reference fails closed in SecretRef.Resolve and every env:/file: reference is
// unaffected (behaviour byte-identical to a deployment that never heard of dyn:). Call once at composition.
// The returned Provider is nil when disabled; when enabled the caller MUST defer Provider.Close to revoke
// leases at shutdown.
func Register(enabled bool, cfg ProviderConfig, logf func(string, ...any)) (*Provider, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if !enabled {
		logf("dyndb: dynamic Postgres credentials OFF (TG_DYNDB_ADDR unset); %q references fail closed, env:/file: DSNs unaffected", Scheme)
		return nil, nil
	}
	p, err := NewProvider(cfg)
	if err != nil {
		return nil, err
	}
	p.logf = logf
	p.eng.SetOnTokenChange(p.requestRotateAll)
	config.RegisterSchemeResolver(Scheme, p.Resolve)
	// TG-545: verify at BOOT that the engine's own token can actually be kept alive. A token provisioned
	// without a period (or non-renewable) ages out at max_ttl no matter how often renew-self runs, after which
	// every dyn: mint fails closed and the plane loses its DB — the 2026-08-26 ~3.5h triage outage, which was
	// silent until it happened. Best-effort: a lookup error must not block boot (the keep-alive loop retries),
	// but a token we CAN see is mis-shaped, or dead, is called out loudly here. In cert mode this lookup is
	// also the first login.
	self, lerr := p.eng.LookupSelf(p.rootCtx)
	switch role := p.eng.CertRole(); {
	case lerr != nil && role == "" && IsForbidden(lerr):
		logf("dyndb: ALERT the engine token is DEAD (lookup-self 403) — every dyn: mint will fail closed (the 2026-09-18 outage shape). %s", p.cureHint(lerr))
	case lerr != nil && IsLoginRefused(lerr):
		logf("dyndb: ALERT the engine's cert login is REFUSED (%v) — every dyn: mint will fail closed until it is fixed. %s", lerr, p.cureHint(lerr))
	case lerr != nil:
		logf("dyndb: could not verify the engine identity at boot (lookup-self: %v) — the keep-alive loop retries every %s. %s", lerr, TokenCheckEvery, p.cureHint(lerr))
	case role != "" && (!self.Renewable || self.Period <= 0):
		logf("dyndb: WARNING cert role %q issues a token that is NOT keep-alive-able (renewable=%v period=%s ttl=%s) — it ages out and every lease minted under it dies with it; set token_period on the role (deploy/openbao/README.md)", role, self.Renewable, self.Period, self.TTL)
	case role != "":
		logf("dyndb: engine identity = OpenBao cert role %q — logged in (periodic %s, ttl %s); a dead token is replaced by logging in again, so no outage length can strand the boot (TG-565)", role, self.Period, self.TTL)
	case !self.Renewable || self.Period <= 0:
		logf("dyndb: WARNING the engine token is NOT keep-alive-able (renewable=%v period=%s ttl=%s) — renew-self CANNOT stop it ageing out at max_ttl, after which every dyn: mint fails closed and the plane loses its DB (the TG-545 outage). %s", self.Renewable, self.Period, self.TTL, p.cureHint(nil))
	default:
		logf("dyndb: engine token verified keep-alive-able at boot — periodic %s, ttl %s, renewable (TG-545 self-check); it is a STORED token: an outage longer than its period strands the boot (TG-565 — prefer TG_DYNDB_CERT_ROLE)", self.Period, self.TTL)
	}
	go p.maintainToken(p.rootCtx, TokenCheckEvery, logf)
	logf("dyndb: dynamic Postgres credentials ON; %q references now lease from OpenBao's database engine "+
		"(rotated at max_ttl and on every engine-token change, token checked every %s and renewed at 3/4 of its period, revoked at shutdown)", Scheme, TokenCheckEvery)
	return p, nil
}
