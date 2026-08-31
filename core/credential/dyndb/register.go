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

	mu       sync.Mutex
	managers map[string]*Manager
	closed   bool
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

// renewFailureEscalateAfter is how many CONSECUTIVE renew-self failures turn the per-tick "will retry" note
// into a loud escalation: a single miss is transient, but a sustained run means the token is dying and the
// plane is heading for a fail-closed DB outage (TG-545) that used to surface only as "failed mints" hours
// later with no proximate cause.
const renewFailureEscalateAfter = 3

// maintainToken keeps the engine's OWN bao token renewed (renew-self) until ctx is cancelled — the token is
// periodic, and nothing else renews it (TG-422 slice 2). A single failed renewal is logged and retried next
// tick; PERSISTENT failure is escalated loudly (TG-545) rather than left to surface downstream as
// unexplained failed mints once the lease finally runs out.
func (p *Provider) maintainToken(ctx context.Context, every time.Duration, logf func(string, ...any)) {
	if every <= 0 {
		every = 6 * time.Hour
	}
	t := time.NewTicker(every)
	defer t.Stop()
	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.eng.RenewSelf(ctx); err != nil {
				consecutiveFailures++
				if consecutiveFailures >= renewFailureEscalateAfter {
					logf("dyndb: ALERT token renew-self has FAILED %d consecutive times (%v) — the engine token is not being kept alive; when it ages out, every dyn: mint fails closed and the plane loses its DB (the TG-545 outage). Check the token is periodic and the OpenBao policy still carves out auth/token/renew-self.", consecutiveFailures, err)
				} else {
					logf("dyndb: token renew-self failed (will retry, %d consecutive): %v", consecutiveFailures, err)
				}
			} else {
				if consecutiveFailures >= renewFailureEscalateAfter {
					logf("dyndb: token renew-self RECOVERED after %d consecutive failures", consecutiveFailures)
				}
				consecutiveFailures = 0
			}
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
	config.RegisterSchemeResolver(Scheme, p.Resolve)
	// TG-545: verify at BOOT that the engine's own token can actually be kept alive. A token provisioned
	// without -period (or non-renewable) ages out at max_ttl no matter how often renew-self runs, after which
	// every dyn: mint fails closed and the plane loses its DB — the 2026-08-26 ~3.5h triage outage, which was
	// silent until it happened. Best-effort: a lookup error must not block boot (renew-self is still
	// scheduled), but a token we CAN see is mis-shaped is called out loudly here.
	if self, lerr := p.eng.LookupSelf(p.rootCtx); lerr != nil {
		logf("dyndb: could not verify the engine token at boot (lookup-self: %v) — renew-self is still scheduled, but a mis-provisioned token would not be caught until it ages out (TG-545)", lerr)
	} else if !self.Renewable || self.Period <= 0 {
		logf("dyndb: WARNING the engine token is NOT keep-alive-able (renewable=%v period=%s ttl=%s) — renew-self CANNOT stop it ageing out at max_ttl, after which every dyn: mint fails closed and the plane loses its DB (the TG-545 outage). Re-mint periodic: bao token create -policy=tg-dyndb-postgres -period=24h", self.Renewable, self.Period, self.TTL)
	} else {
		logf("dyndb: engine token verified keep-alive-able at boot — periodic %s, ttl %s, renewable (TG-545 self-check)", self.Period, self.TTL)
	}
	go p.maintainToken(p.rootCtx, 6*time.Hour, logf)
	logf("dyndb: dynamic Postgres credentials ON; %q references now lease from OpenBao's database engine "+
		"(rotated at max_ttl, token renew-self every 6h, revoked at shutdown)", Scheme)
	return p, nil
}
