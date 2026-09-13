package dyndb

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Manager keeps ONE database role's lease alive across its TTL: it issues an initial credential (Start),
// renews it before the TTL elapses (Maintain, or step for a caller/oracle driving the clock), exposes the
// current credential (Current), and revokes it on Close. This is the "value with a lifecycle" the static
// SecretRef schemes (env:/file:/store:) lack — TG-422 point 1.
//
// FAIL-CLOSED ON EXPIRY: if renewal stops succeeding (substrate unreachable), Current returns ok=false once
// the held lease's TTL has passed, rather than handing out a credential the engine has already expired. A
// stale credential is exactly the silent fallback this package refuses.
type Manager struct {
	eng  *Engine
	role string
	now  func() time.Time
	// renewFraction is the fraction of the TTL at which renewal becomes due (0.66 → renew with ~a third of
	// the lease still valid, leaving headroom to retry before the credential actually expires).
	renewFraction float64

	mu     sync.Mutex
	lease  *Lease
	expiry time.Time
	closed bool
	// onRotate, if set, fires once each time step() rotates the lease (a fresh lease replaces the old one,
	// which is then revoked/dropped). The composition root wires pool.Reset here so pooled connections dialed
	// under the OLD lease are recycled the instant it rotates — a dropped lease role's live session is NOT
	// killed but goes UNPRIVILEGED (TG-553). nil until SetOnRotate; read under mu, called outside it.
	onRotate func()
}

// ManagerOption customises a Manager (test seams + tuning).
type ManagerOption func(*Manager)

// WithClock injects the time source (default time.Now). Tests advance a fake clock to make renewal
// deterministic without sleeping.
func WithClock(now func() time.Time) ManagerOption {
	return func(m *Manager) {
		if now != nil {
			m.now = now
		}
	}
}

// WithRenewFraction sets the TTL fraction at which renewal becomes due (default 0.66). An out-of-range value
// (not strictly between 0 and 1) is ignored and the default stands.
func WithRenewFraction(f float64) ManagerOption {
	return func(m *Manager) {
		if f > 0 && f < 1 {
			m.renewFraction = f
		}
	}
}

// NewManager builds a Manager for one role. Call Start before Current.
func NewManager(eng *Engine, role string, opts ...ManagerOption) *Manager {
	m := &Manager{eng: eng, role: role, now: time.Now, renewFraction: 0.66}
	for _, o := range opts {
		o(m)
	}
	return m
}

// SetOnRotate registers the callback step() fires after each lease rotation (see the onRotate field and
// Provider.OnRotate). Safe to call after the Manager's Maintain goroutine is already running — the pool it
// evicts is built AFTER the Manager, so the callback can only be wired post-construction. Last writer wins.
func (m *Manager) SetOnRotate(cb func()) {
	m.mu.Lock()
	m.onRotate = cb
	m.mu.Unlock()
}

// Start issues the first lease. Fails closed (and leaves the Manager unusable) if the engine will not mint.
func (m *Manager) Start(ctx context.Context) error {
	l, err := m.eng.Issue(ctx, m.role)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		// Raced with Close — revoke what we just minted rather than leak it.
		_ = m.eng.Revoke(ctx, l.LeaseID)
		return errors.New("dyndb: manager closed")
	}
	m.lease = l
	m.expiry = m.now().Add(l.Duration)
	return nil
}

// Current returns the live username/password. ok is false when the Manager is closed, has not started, or the
// held lease has expired without a successful renew (fail closed).
func (m *Manager) Current() (username, password string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.lease == nil {
		return "", "", false
	}
	if !m.now().Before(m.expiry) {
		return "", "", false
	}
	return m.lease.Username, m.lease.Password, true
}

// renewDue reports whether the lease has passed its renew point (renewFraction of the TTL elapsed). Caller
// holds m.mu.
func (m *Manager) renewDue(now time.Time) bool {
	if m.lease == nil || m.lease.Duration <= 0 {
		return false
	}
	remaining := m.expiry.Sub(now)
	margin := time.Duration(float64(m.lease.Duration) * (1 - m.renewFraction))
	return remaining <= margin
}

// step renews the lease if it is due — and ROTATES it when renewal can no longer carry it (TG-422 slice 2).
// It is the unit Maintain calls each tick and the seam the oracle drives with a fake clock.
//
// Renewal alone cannot keep a lease alive forever: the engine caps every renewal at the role's max_ttl, so
// the granted TTL SHRINKS as the lease approaches its hard ceiling and eventually renewal stops helping.
// Before slice 2 that was the end — Current went dark at max_ttl and the pool lost its credential. Now a
// renewal error, or a grant materially below the requested increment (≤ half), triggers a rotation: mint a
// FRESH lease, swap it in, revoke the old one. Revoking the old lease DROPs its Postgres role — and a dropped
// role does NOT kill its live sessions but it does STRIP them: verified on the live pg16 (TG-553), a still-open
// session whose role was dropped gets "invalid role OID" for current_user and permission-denied on every table.
// (The earlier note here — "sessions continue, only NEW logins die" — saw the first half and missed the second.)
// So a pooled connection dialed under the old lease keeps failing permission-denied until it is recycled: step()
// fires onRotate (the composition root wires pool.Reset) the moment it swaps, BEFORE the revoke, so those
// connections are evicted and re-dialed under the fresh lease via ConnectDynamic's BeforeConnect. If BOTH
// renewal and rotation fail (substrate unreachable), the lease keeps whatever expiry it has and Current fails
// closed once it passes — degraded to "no credential", never "a stale one".
func (m *Manager) step(ctx context.Context, now time.Time) {
	m.mu.Lock()
	var leaseID string
	var dur time.Duration
	due := false
	if !m.closed && m.lease != nil {
		leaseID = m.lease.LeaseID
		dur = m.lease.Duration
		due = m.renewDue(now)
	}
	m.mu.Unlock()
	if !due {
		return
	}
	newTTL, err := m.eng.Renew(ctx, leaseID, dur)
	if err == nil && newTTL > dur/2 {
		m.mu.Lock()
		if !m.closed && m.lease != nil && m.lease.LeaseID == leaseID {
			m.expiry = now.Add(newTTL)
		}
		m.mu.Unlock()
		return
	}
	// Rotate. Issue FIRST — the old lease stays valid while the new one is minted, so a failed mint leaves
	// the Manager exactly where it was (and the next tick retries).
	fresh, ierr := m.eng.Issue(ctx, m.role)
	if ierr != nil {
		if err == nil && newTTL > 0 {
			// Renewal DID grant a remnant (just a short one). Keep it — it is real validity, and the next
			// tick retries the rotation with that headroom instead of none.
			m.mu.Lock()
			if !m.closed && m.lease != nil && m.lease.LeaseID == leaseID {
				m.expiry = now.Add(newTTL)
			}
			m.mu.Unlock()
		}
		return
	}
	m.mu.Lock()
	if m.closed || m.lease == nil || m.lease.LeaseID != leaseID {
		// Closed or replaced while we minted — the fresh lease has no holder. Revoke IT, not the held one.
		m.mu.Unlock()
		_ = m.eng.Revoke(ctx, fresh.LeaseID)
		return
	}
	old := m.lease
	m.lease = fresh
	m.expiry = now.Add(fresh.Duration)
	hook := m.onRotate
	m.mu.Unlock()
	// Evict pool connections dialed under `old` BEFORE it is revoked: a dropped lease role's live session
	// survives the DROP but loses every privilege (TG-553), so a connection that outlived its lease must be
	// recycled, never trusted to keep working. Fired outside the lock — pool.Reset triggers new connections
	// whose BeforeConnect re-enters Current, which takes m.mu.
	if hook != nil {
		hook()
	}
	_ = m.eng.Revoke(ctx, old.LeaseID)
}

// Maintain blocks, renewing the lease before each TTL boundary, until ctx is cancelled. The composition root
// runs it in a goroutine; tick is how often the renewal decision is evaluated (should be a fraction of the
// lease TTL). Close (via ctx cancel) stops it; it does not itself revoke — Close does.
func (m *Manager) Maintain(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = time.Minute
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.step(ctx, m.now())
		}
	}
}

// Close revokes the held lease and marks the Manager closed. After Close, Current returns ok=false.
// Idempotent. Uses ctx for the revoke call, so a shutdown with a deadline still attempts the revoke.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	l := m.lease
	m.lease = nil
	m.mu.Unlock()
	if l == nil || l.LeaseID == "" {
		return nil
	}
	return m.eng.Revoke(ctx, l.LeaseID)
}
