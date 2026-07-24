// Browser operator sessions (spec/006 REQ-508): the third authentication method beside HMAC and mTLS,
// so a human operator's browser can consume the READ-ONLY console surface. The design keeps INV-01's
// two structural guarantees intact and adds a third:
//
//  3. A session principal is read-only BY CONSTRUCTION — it can only ever satisfy a route registered
//     AuthReadOnly, and only for safe (GET) requests. It can never satisfy an AuthHMAC or AuthMTLS
//     route (ingest, replay, any future mutation surface): those routes never even inspect a cookie.
//
// Provenance: [O] INV-01 (mandatory auth, default-deny), [R] spec/010 (the console is the consumer)
// · guardrail "no literal secrets" (operator tokens and the signing key arrive as SecretRef values,
// and only a SHA-256 digest of the operator token is held in memory).
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SessionCookieName is the browser session cookie. One name, one place.
const SessionCookieName = "tg_session"

var (
	// ErrSessionUnconfigured fails closed when a session-authenticated route is served by a verifier
	// with no session authenticator — the browser path simply does not exist until it is configured.
	ErrSessionUnconfigured = errors.New("auth: browser sessions not configured")
	// ErrLoginRateLimited rejects a login attempt over the failure budget (429 at the surface).
	ErrLoginRateLimited = errors.New("auth: operator login rate-limited")
)

// SessionStore is the server-side session registry: a cookie authenticates only if its id is present
// here and unexpired, so revocation (logout) is authoritative regardless of what the browser holds.
type SessionStore interface {
	Put(ctx context.Context, id, operator string, expires time.Time) error
	// Get returns found=false for an unknown OR revoked id — observationally identical (REQ-504 spirit).
	Get(ctx context.Context, id string) (operator string, expires time.Time, found bool, err error)
	Revoke(ctx context.Context, id string) error
}

// MemSessionStore is the in-memory store: the CI oracle fake AND the Phase-1 single-instance store.
type MemSessionStore struct {
	mu   sync.RWMutex
	rows map[string]memSession
}

type memSession struct {
	operator string
	expires  time.Time
}

// NewMemSessionStore builds an empty in-memory session store.
func NewMemSessionStore() *MemSessionStore { return &MemSessionStore{rows: map[string]memSession{}} }

// Put registers a session id.
func (s *MemSessionStore) Put(_ context.Context, id, operator string, expires time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[id] = memSession{operator: operator, expires: expires}
	return nil
}

// Get resolves a session id; unknown and revoked are the same "not found".
func (s *MemSessionStore) Get(_ context.Context, id string) (string, time.Time, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	row, ok := s.rows[id]
	if !ok {
		return "", time.Time{}, false, nil
	}
	return row.operator, row.expires, true, nil
}

// Revoke deletes a session id; revoking an unknown id is a no-op.
func (s *MemSessionStore) Revoke(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, id)
	return nil
}

// Operator is a registered human operator. Only the SHA-256 digest of the login token is ever held —
// the token value itself is compared and discarded (no literal secret at rest in process state).
type Operator struct {
	Name        string
	TokenSHA256 [sha256.Size]byte
}

// OperatorResolver looks up an operator by name (single-org, ADR-0010).
type OperatorResolver interface {
	LookupOperator(ctx context.Context, name string) (Operator, error)
}

// MemOperators is a fixed in-memory operator set (the Phase-1 composition and the oracle fake).
type MemOperators map[string]Operator

// LookupOperator resolves a configured operator; unknown names fail like a bad token (uniform error).
func (m MemOperators) LookupOperator(_ context.Context, name string) (Operator, error) {
	op, ok := m[name]
	if !ok {
		return Operator{}, ErrUnauthenticated
	}
	return op, nil
}

// loginLimiter is a fixed-window failure budget per (operator, remote-ip): over budget, login attempts
// are refused BEFORE any digest comparison. In-memory and deliberately simple — its job is to blunt
// online guessing, not to be a distributed rate-limit fabric.
type loginLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	rows   map[string]loginWindow
}

type loginWindow struct {
	start time.Time
	fails int
}

func newLoginLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{max: max, window: window, rows: map[string]loginWindow{}}
}

// blocked reports whether the key is over its failure budget for the current window.
func (l *loginLimiter) blocked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.rows[key]
	if !ok || now.Sub(w.start) > l.window {
		return false
	}
	return w.fails >= l.max
}

// fail records a failed attempt (opening a fresh window when the old one lapsed).
func (l *loginLimiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.rows[key]
	if !ok || now.Sub(w.start) > l.window {
		l.rows[key] = loginWindow{start: now, fails: 1}
		return
	}
	w.fails++
	l.rows[key] = w
}

// reset clears the failure window after a successful login.
func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.rows, key)
}

// SessionAuthenticator issues and verifies browser operator sessions. The cookie value is
// "<id>.<hex hmac-sha256(key, id)>": the signature proves the id was minted here, the server-side
// store makes revocation authoritative, and the TTL bounds the session's life. All three checks must
// pass — fail closed.
type SessionAuthenticator struct {
	key       []byte
	store     SessionStore
	operators OperatorResolver
	ttl       time.Duration
	limiter   *loginLimiter
	// Secure controls the cookie's Secure attribute (true in production behind TLS; oracles may relax).
	Secure bool
	// now is injectable for the oracles; defaults to time.Now.
	now func() time.Time

	// ldap, when set (EnableLDAP), authenticates a NON-break-glass login by binding as the user against
	// FreeIPA (the token is the user's PASSWORD) and mapping their group membership to a role — a fourth
	// login method composed WITH the static operator+token, never weakening it (REQ-508). nil = LDAP off.
	ldap *LDAPAuthenticator
	// breakGlass is the static operator name that ALWAYS uses the digest/token path (the break-glass
	// bootstrap login), so a misconfigured/unreachable LDAP can never lock everyone out. Every other name
	// uses the LDAP path when ldap != nil.
	breakGlass string
	// adminEligible records the server-side session ids minted for an LDAP admin-group (tg-admins) user:
	// such a session may STEP UP to the admin write tier on group membership alone, without the separate
	// static admin credential (REQ-522). Cleared on logout and pruned lazily. No secret is held here.
	elMu          sync.Mutex
	adminEligible map[string]struct{}
}

// NewSessionAuthenticator constructs the browser-session authenticator and fails closed at
// construction on any missing piece: a short key, a nil store or resolver, or a non-positive TTL is a
// boot-time error, never a silently weaker runtime.
func NewSessionAuthenticator(key []byte, store SessionStore, operators OperatorResolver, ttl time.Duration) (*SessionAuthenticator, error) {
	if len(key) < 32 {
		return nil, errors.New("auth: session key must be at least 32 bytes (supply via a secret reference)")
	}
	if store == nil || operators == nil || ttl <= 0 {
		return nil, errors.New("auth: browser sessions need a session store, an operator resolver, and a positive TTL")
	}
	return &SessionAuthenticator{
		key:           key,
		store:         store,
		operators:     operators,
		ttl:           ttl,
		limiter:       newLoginLimiter(5, time.Minute),
		Secure:        true,
		now:           time.Now,
		adminEligible: map[string]struct{}{},
	}, nil
}

// EnableLDAP attaches an LDAP authenticator and names the static break-glass operator. Once enabled,
// every login whose name is NOT the break-glass operator is authenticated by binding as the user against
// FreeIPA (the token is the user's password) and mapping their group to a role; the break-glass operator
// keeps the static digest/token path so an unreachable LDAP can never lock everyone out. Passing a nil
// authenticator is a no-op (LDAP stays off).
func (s *SessionAuthenticator) EnableLDAP(l *LDAPAuthenticator, breakGlassName string) {
	if l == nil {
		return
	}
	s.ldap = l
	s.breakGlass = breakGlassName
}

// AdminEligible reports whether the server-side session id was minted for an LDAP admin-group member,
// making it eligible to step up to the admin write tier without the static admin credential (REQ-522).
func (s *SessionAuthenticator) AdminEligible(id string) bool {
	s.elMu.Lock()
	defer s.elMu.Unlock()
	_, ok := s.adminEligible[id]
	return ok
}

func (s *SessionAuthenticator) markAdminEligible(id string) {
	s.elMu.Lock()
	defer s.elMu.Unlock()
	s.adminEligible[id] = struct{}{}
}

func (s *SessionAuthenticator) clearAdminEligible(id string) {
	s.elMu.Lock()
	defer s.elMu.Unlock()
	delete(s.adminEligible, id)
}

// WithClock overrides the clock (oracle use).
func (s *SessionAuthenticator) WithClock(now func() time.Time) *SessionAuthenticator {
	if now != nil {
		s.now = now
	}
	return s
}

func (s *SessionAuthenticator) sign(id string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(id))
	return hex.EncodeToString(mac.Sum(nil))
}

// Login authenticates an operator name + token (rate-limited per operator+ip) and mints a registered
// session cookie on success. Two composed paths, chosen by name — never falling through from a stronger
// to a weaker check:
//   - LDAP is enabled AND name is not the break-glass operator: bind AS THE USER against FreeIPA (the
//     token is the user's PASSWORD, never stored/hashed/logged) and map their group membership to a role.
//     Bind failure, an unreachable directory, or membership in no allowed TG group all fail closed.
//   - otherwise (break-glass operator, or LDAP off): the static digest/token path (constant-time compare
//     against the resolver's stored SHA-256), unchanged.
//
// An LDAP admin-group (tg-admins) login additionally marks the minted session admin-eligible, so it may
// later step up to the admin write tier on group membership alone (REQ-522).
func (s *SessionAuthenticator) Login(ctx context.Context, name, token, remoteAddr string) (*http.Cookie, time.Time, error) {
	now := s.now()
	ip := remoteAddr
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		ip = host
	}
	key := name + "|" + ip
	if s.limiter.blocked(key, now) {
		return nil, time.Time{}, ErrLoginRateLimited
	}

	var adminEligible bool
	if s.ldap != nil && name != s.breakGlass {
		// LDAP path: a successful bind IS the identity proof; a DENY/error never falls through to the
		// static path. name/token emptiness is caught inside Authenticate (fail closed).
		role, err := s.ldap.Authenticate(ctx, name, token)
		if err != nil || role == LDAPDeny {
			s.limiter.fail(key, now)
			return nil, time.Time{}, ErrUnauthenticated
		}
		adminEligible = role == LDAPAdmin
	} else {
		op, err := s.operators.LookupOperator(ctx, name)
		digest := sha256.Sum256([]byte(token))
		// Compare even on lookup failure so unknown-operator and bad-token cost the same.
		match := subtle.ConstantTimeCompare(digest[:], op.TokenSHA256[:]) == 1
		if err != nil || !match || name == "" || token == "" {
			s.limiter.fail(key, now)
			return nil, time.Time{}, ErrUnauthenticated
		}
	}
	s.limiter.reset(key)

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, time.Time{}, err
	}
	id := hex.EncodeToString(raw)
	expires := now.Add(s.ttl)
	if err := s.store.Put(ctx, id, name, expires); err != nil {
		return nil, time.Time{}, err
	}
	if adminEligible {
		s.markAdminEligible(id)
	}
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    id + "." + s.sign(id),
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteStrictMode,
	}, expires, nil
}

// verify authenticates a request's session cookie: signature, registry presence, and expiry must all
// hold. Any failure is ErrUnauthenticated — tampered, expired, revoked, and absent are observationally
// identical to the caller.
func (s *SessionAuthenticator) verify(r *http.Request) (string, error) {
	_, operator, err := s.verifyWithID(r)
	return operator, err
}

// verifyWithID is verify exposing the server-side session id too — the key the admin step-up tier
// (task #27 Phase B) elevates. The id is never returned to a caller; it stays inside core/auth.
func (s *SessionAuthenticator) verifyWithID(r *http.Request) (string, string, error) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil || c.Value == "" {
		return "", "", ErrUnauthenticated
	}
	id, sig, ok := strings.Cut(c.Value, ".")
	if !ok || id == "" || sig == "" {
		return "", "", ErrUnauthenticated
	}
	want := s.sign(id)
	if subtle.ConstantTimeCompare([]byte(want), []byte(sig)) != 1 {
		return "", "", ErrUnauthenticated
	}
	operator, expires, found, err := s.store.Get(r.Context(), id)
	if err != nil || !found || s.now().After(expires) {
		return "", "", ErrUnauthenticated
	}
	return id, operator, nil
}

// RevokeRequest revokes the session carried by the request's cookie (logout). Verifying first means a
// forged cookie cannot revoke someone else's session id.
func (s *SessionAuthenticator) RevokeRequest(r *http.Request) error {
	if _, err := s.verify(r); err != nil {
		return err
	}
	c, _ := r.Cookie(SessionCookieName)
	id, _, _ := strings.Cut(c.Value, ".")
	s.clearAdminEligible(id) // an LDAP admin session forfeits its step-up eligibility on logout
	return s.store.Revoke(r.Context(), id)
}

// --- Verifier integration -------------------------------------------------------------------------

// EnableBrowserSessions attaches the browser-session authenticator to a verifier. Without this call,
// session- and login-class routes fail closed with ErrSessionUnconfigured.
func (v *Verifier) EnableBrowserSessions(sa *SessionAuthenticator) {
	v.sessions = sa
}

// authenticateSession produces a read-only session principal from a valid session cookie.
func (v *Verifier) authenticateSession(r *http.Request) (Principal, error) {
	if v.sessions == nil {
		return Principal{}, ErrSessionUnconfigured
	}
	operator, err := v.sessions.verify(r)
	if err != nil {
		return Principal{}, err
	}
	return Principal{SourceID: "operator:" + operator, Method: AuthSession}, nil
}

// authenticateOperatorLogin authenticates a login attempt (X-TG-Operator + Authorization: Bearer) and
// mints the session as a side effect, exposing the pending cookie to the handler via the request
// context. The handler's only job is to set the cookie and report the expiry.
func (v *Verifier) authenticateOperatorLogin(r *http.Request) (Principal, *http.Cookie, time.Time, error) {
	if v.sessions == nil {
		return Principal{}, nil, time.Time{}, ErrSessionUnconfigured
	}
	name := r.Header.Get("X-TG-Operator")
	authz := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(authz, "Bearer ")
	if name == "" || !ok || token == "" {
		return Principal{}, nil, time.Time{}, ErrUnauthenticated
	}
	cookie, expires, err := v.sessions.Login(r.Context(), name, token, r.RemoteAddr)
	if err != nil {
		return Principal{}, nil, time.Time{}, err
	}
	return Principal{SourceID: "operator:" + name, Method: AuthOperatorLogin}, cookie, expires, nil
}

type sessionCookieCtxKey struct{}

// PendingSessionCookie returns the cookie minted by the login authentication, for the login handler.
func PendingSessionCookie(ctx context.Context) (*http.Cookie, bool) {
	c, ok := ctx.Value(sessionCookieCtxKey{}).(*http.Cookie)
	return c, ok
}
