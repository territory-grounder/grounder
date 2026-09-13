// Admin operator tier (task #27 Phase B, spec/006 REQ-522): a SECOND, stronger operator lane for the
// control-plane WRITE surfaces (config overrides, sealed secrets). The signed-off design: the admin
// lane is a NEW structural auth class — AuthAdminSession — obtained ONLY by STEP-UP re-authentication
// on top of a valid operator session. The separate admin credential (name + token, supplied via secret
// references; only a SHA-256 digest held in process) is presented to the elevation route, which marks
// the SERVER-SIDE session admin-elevated for a bounded, short TTL. A plain session, a machine
// principal, an expired elevation, and an absent credential are all one indistinguishable 401.
//
// Fail-closed by construction: an unconfigured AdminAuthenticator means the elevation route and every
// AuthAdminSession route are NOT REGISTERED at all (mirroring the browser-session pattern), and a
// grounder restart drops all elevations (they are in-process state with a short TTL — an operator
// simply re-elevates). Elevation piggybacks the existing session cookie; no second cookie exists, so
// the admin tier adds zero new bearer material to the browser.
//
// Provenance: [O] INV-01 (default-deny; the new tier weakens no existing lane) · guardrail "no literal
// secrets" (the admin token arrives by reference, digest-only in memory) · task #27 signed-off design
// ("AuthAdminSession = valid session + admin capability; step-up re-auth; short TTL; rate limit").
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrAdminUnconfigured fails closed when an admin-class route is served by a verifier with no admin
// authenticator — the admin tier simply does not exist until it is configured.
var ErrAdminUnconfigured = errors.New("auth: admin sessions not configured")

// adminElevation is one live step-up grant: which admin credential elevated the session, until when.
type adminElevation struct {
	admin   string
	expires time.Time
}

// AdminAuthenticator grants and verifies short-lived admin elevations keyed by the server-side
// session id. It holds NO credential material beyond the digest-only OperatorResolver.
type AdminAuthenticator struct {
	admins  OperatorResolver
	ttl     time.Duration
	limiter *loginLimiter

	mu       sync.Mutex
	elevated map[string]adminElevation

	// now is injectable for the oracles; defaults to time.Now.
	now func() time.Time
}

// NewAdminAuthenticator constructs the admin step-up authenticator and fails closed at construction:
// a nil resolver or a non-positive TTL is a boot-time error, never a silently weaker runtime.
func NewAdminAuthenticator(admins OperatorResolver, ttl time.Duration) (*AdminAuthenticator, error) {
	if admins == nil || ttl <= 0 {
		return nil, errors.New("auth: admin sessions need an admin resolver and a positive TTL")
	}
	return &AdminAuthenticator{
		admins:   admins,
		ttl:      ttl,
		limiter:  newLoginLimiter(5, time.Minute),
		elevated: map[string]adminElevation{},
		now:      time.Now,
	}, nil
}

// WithClock overrides the clock (oracle use).
func (a *AdminAuthenticator) WithClock(now func() time.Time) *AdminAuthenticator {
	if now != nil {
		a.now = now
	}
	return a
}

// TTL reports the configured elevation lifetime (for surfaces that state it honestly).
func (a *AdminAuthenticator) TTL() time.Duration { return a.ttl }

// Elevate verifies the admin credential (constant time, rate-limited per admin+ip) and marks the
// ALREADY-VERIFIED session id admin-elevated until now+TTL. The session id is trusted here exactly
// because the caller (the Verifier) only passes an id that survived full cookie verification.
func (a *AdminAuthenticator) Elevate(ctx context.Context, sessionID, adminName, token, remoteAddr string) (time.Time, error) {
	now := a.now()
	ip := remoteAddr
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		ip = host
	}
	key := adminName + "|" + ip
	if a.limiter.blocked(key, now) {
		return time.Time{}, ErrLoginRateLimited
	}
	op, err := a.admins.LookupOperator(ctx, adminName)
	digest := sha256.Sum256([]byte(token))
	// Compare even on lookup failure so unknown-admin and bad-token cost the same.
	match := subtle.ConstantTimeCompare(digest[:], op.TokenSHA256[:]) == 1
	if err != nil || !match || sessionID == "" || adminName == "" || token == "" {
		a.limiter.fail(key, now)
		return time.Time{}, ErrUnauthenticated
	}
	a.limiter.reset(key)

	expires := now.Add(a.ttl)
	a.mu.Lock()
	defer a.mu.Unlock()
	// Lazy prune: expired grants of revoked/abandoned sessions never accumulate.
	for id, e := range a.elevated {
		if now.After(e.expires) {
			delete(a.elevated, id)
		}
	}
	a.elevated[sessionID] = adminElevation{admin: adminName, expires: expires}
	return expires, nil
}

// ElevateEligible marks an ALREADY-VERIFIED, LDAP-admin-eligible session (a tg-admins member's login,
// established at login time) admin-elevated for the bounded TTL — WITHOUT a separate static admin
// credential. The group membership IS the authorization; there is no secret to compare or guess here, so
// no rate-limit is applied. The caller (the Verifier) only reaches this with a session that survived full
// cookie verification AND SessionAuthenticator.AdminEligible — it never trusts a caller-supplied id.
func (a *AdminAuthenticator) ElevateEligible(sessionID, operator string) (time.Time, error) {
	if sessionID == "" {
		return time.Time{}, ErrUnauthenticated
	}
	now := a.now()
	expires := now.Add(a.ttl)
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, e := range a.elevated { // lazy prune, mirroring Elevate
		if now.After(e.expires) {
			delete(a.elevated, id)
		}
	}
	a.elevated[sessionID] = adminElevation{admin: operator, expires: expires}
	return expires, nil
}

// Elevated reports whether the session id holds a live (unexpired) admin elevation, and which admin
// credential granted it. An expired grant is deleted and reported absent — fail closed.
func (a *AdminAuthenticator) Elevated(sessionID string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.elevated[sessionID]
	if !ok {
		return "", false
	}
	if a.now().After(e.expires) {
		delete(a.elevated, sessionID)
		return "", false
	}
	return e.admin, true
}

// Drop revokes an elevation (session logout hygiene); dropping an unknown id is a no-op.
func (a *AdminAuthenticator) Drop(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.elevated, sessionID)
}

// --- Verifier integration -------------------------------------------------------------------------

// EnableAdminSessions attaches the admin step-up authenticator to a verifier. Without this call,
// elevate- and admin-class routes fail closed with ErrAdminUnconfigured.
func (v *Verifier) EnableAdminSessions(aa *AdminAuthenticator) {
	v.admins = aa
}

// AdminHeaderName carries the admin account name on the elevation route (the token rides the
// standard Authorization: Bearer header, mirroring the operator login route).
const AdminHeaderName = "X-TG-Admin"

// authenticateAdminElevate performs the step-up: the session cookie must verify (the caller IS an
// authenticated operator), and the separate admin credential must verify (the caller ALSO holds the
// admin secret). Success marks the session elevated and reports the grant expiry.
func (v *Verifier) authenticateAdminElevate(r *http.Request) (Principal, time.Time, error) {
	if v.sessions == nil || v.admins == nil {
		return Principal{}, time.Time{}, ErrAdminUnconfigured
	}
	id, operator, err := v.sessions.verifyWithID(r)
	if err != nil {
		return Principal{}, time.Time{}, ErrUnauthenticated
	}
	// LDAP admin-eligible session (a tg-admins member): group membership, proven at login, authorizes the
	// step-up — no separate static admin credential is required (REQ-522). A non-eligible session (a plain
	// operator, or a static break-glass operator login) falls to the static-credential path below.
	if v.sessions.AdminEligible(id) {
		until, err := v.admins.ElevateEligible(id, operator)
		if err != nil {
			return Principal{}, time.Time{}, ErrUnauthenticated
		}
		return Principal{SourceID: "operator:" + operator, Method: AuthAdminElevate, Admin: true}, until, nil
	}
	name := r.Header.Get(AdminHeaderName)
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if name == "" || !ok || token == "" {
		return Principal{}, time.Time{}, ErrUnauthenticated
	}
	until, err := v.admins.Elevate(r.Context(), id, name, token, r.RemoteAddr)
	if err != nil {
		return Principal{}, time.Time{}, err
	}
	return Principal{SourceID: "operator:" + operator, Method: AuthAdminElevate, Admin: true}, until, nil
}

// authenticateAdminSession admits only a valid session with a live admin elevation (REQ-522). It
// never inspects HMAC/mTLS material — a machine principal structurally cannot reach an admin route.
func (v *Verifier) authenticateAdminSession(r *http.Request) (Principal, error) {
	if v.sessions == nil || v.admins == nil {
		return Principal{}, ErrAdminUnconfigured
	}
	id, operator, err := v.sessions.verifyWithID(r)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	if _, ok := v.admins.Elevated(id); !ok {
		return Principal{}, ErrUnauthenticated
	}
	return Principal{SourceID: "operator:" + operator, Method: AuthAdminSession, Admin: true}, nil
}

type adminGrantCtxKey struct{}

// PendingAdminGrant returns the elevation expiry minted by the elevate authentication, for the
// elevate handler's response. Absent on every other route.
func PendingAdminGrant(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(adminGrantCtxKey{}).(time.Time)
	return t, ok
}
