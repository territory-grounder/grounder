// Package auth implements Territory Grounder's mandatory, non-bypassable ingress authentication.
//
// Provenance: [O] INV-01 (every ingress authenticated, default-deny by construction), H-01/H-09
// (the predecessor exported ~25 unauthenticated webhooks), P0-2. Threat: "unauthenticated actor
// triggers any receiver".
//
// Two structural guarantees:
//  1. A route CANNOT be registered without an auth method — Handle panics at boot on AuthNone, so a
//     forgotten config yields a DEAD endpoint, never an open one.
//  2. A handler CANNOT run without an authenticated Principal — the handler signature requires one,
//     and the only way to obtain it is through the middleware that verified the caller BEFORE the
//     handler (and before the handler parses the body).
package auth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// AuthMethod is how a route authenticates callers. The zero value is AuthNone, which is INVALID for
// registration — this makes "no auth" a boot-time panic rather than a silent open endpoint.
type AuthMethod int

const (
	AuthNone AuthMethod = iota // zero value — INVALID for a route; panics at registration
	AuthHMAC                   // per-source HMAC over raw body with timestamp + nonce
	AuthMTLS                   // verified mTLS client certificate
	// AuthSession is the browser operator session (REQ-508): as a principal method it marks a
	// read-only human caller; as a ROUTE method it admits only a valid session cookie (logout).
	AuthSession
	// AuthOperatorLogin is a ROUTE-only method: it authenticates an operator name + bearer token and
	// mints the session as part of authentication (the login route). Never a principal on any other route.
	AuthOperatorLogin
	// AuthReadOnly is a ROUTE-only class for pure-read surfaces: a machine principal (HMAC/mTLS)
	// satisfies it as before, and a session principal satisfies it for safe (GET) requests ONLY.
	// Session principals can never satisfy AuthHMAC/AuthMTLS routes — those never inspect a cookie.
	AuthReadOnly
	// AuthIngestPush is a ROUTE-only class for the ingest front door: a machine caller (HMAC/mTLS) satisfies
	// it exactly as AuthHMAC (tried FIRST, unchanged), and ONLY when no HMAC/mTLS credential is present is a
	// per-source STATIC bearer token accepted — for push sources (e.g. Alertmanager webhooks) that cannot
	// HMAC-sign the body. The bearer path is fail-closed, constant-time, and per-source; it NEVER weakens the
	// HMAC/mTLS path. A static token has no replay protection, so an AuthIngestPush route MUST be TLS-fronted.
	AuthIngestPush
	// AuthAdminElevate is a ROUTE-only method (task #27 Phase B): step-up re-authentication. It admits
	// ONLY a caller holding a valid operator session cookie who ALSO presents the separate admin
	// credential (X-TG-Admin + Authorization: Bearer); authentication marks the server-side session
	// admin-elevated for a bounded TTL. Never a principal on any other route.
	AuthAdminElevate
	// AuthAdminSession is the admin operator tier (task #27 Phase B, REQ-522): the route class for
	// control-plane WRITE surfaces. It admits only a valid session that is CURRENTLY admin-elevated
	// (step-up re-auth, bounded TTL); the principal carries Admin=true. A machine credential or a plain
	// session can never satisfy it, and an unconfigured admin authenticator means routes of this class
	// are never registered at all (fail closed).
	AuthAdminSession
	// AuthTraceRead is the elevated decision-tracer read tier (spec/020 REQ-2014): STRICTLY more elevated
	// than AuthReadOnly. A machine principal (HMAC/mTLS) satisfies it as a trusted system caller, tried
	// FIRST and unchanged; but a browser SESSION satisfies it ONLY when its server-side session is
	// admin-eligible (an LDAP tg-admins member — the SAME elevated tier the admin write step-up recognizes).
	// A PLAIN read-only operator session — one that satisfies AuthReadOnly — is REFUSED here (403), so the
	// tracer's decision internals (rules, rationale, gate verdicts, agent steps) are gated to elevated
	// operators, not the whole read-only console surface. Fail-closed: no session authenticator, a
	// non-eligible session, or a non-GET request is refused.
	AuthTraceRead
)

func (m AuthMethod) String() string {
	switch m {
	case AuthHMAC:
		return "hmac"
	case AuthMTLS:
		return "mtls"
	case AuthSession:
		return "session"
	case AuthOperatorLogin:
		return "operator-login"
	case AuthReadOnly:
		return "read-only"
	case AuthIngestPush:
		return "ingest-push"
	case AuthAdminElevate:
		return "admin-elevate"
	case AuthAdminSession:
		return "admin-session"
	case AuthTraceRead:
		return "trace-read"
	default:
		return "none"
	}
}

// Principal is a verified caller identity. It exists only after successful authentication.
type Principal struct {
	SourceID string
	Method   AuthMethod
	Admin    bool
}

type ctxKey struct{}

// FromContext returns the authenticated principal, if any.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Source is a registered caller: its id, its per-source HMAC secret, and an optional per-source static
// bearer token for the ingest-push path.
type Source struct {
	SourceID   string
	HMACSecret []byte
	// IngestToken is an OPTIONAL per-source static bearer token for AuthIngestPush — used by push sources
	// that cannot compute an HMAC (e.g. Alertmanager). Empty ⇒ the bearer path fails closed for this source.
	// It is resolved by reference (INV-13), never stored as a literal.
	IngestToken []byte
}

// SourceResolver looks up a source by id (per-source HMAC credentials; single-org, ADR-0010).
type SourceResolver interface {
	LookupSource(ctx context.Context, sourceID string) (Source, error)
}

// NonceStore records seen (source,nonce) pairs to defeat replay. A true return means "already seen".
type NonceStore interface {
	SeenBefore(ctx context.Context, sourceID, nonce string, ts time.Time) (bool, error)
}

// Verifier authenticates requests. Window bounds acceptable clock skew for HMAC timestamps.
type Verifier struct {
	Sources SourceResolver
	Nonces  NonceStore
	Window  time.Duration
	// sessions is the optional browser-session authenticator (REQ-508). nil = the browser path does
	// not exist: session/login-class routes fail closed; machine routes are entirely unaffected.
	sessions *SessionAuthenticator
	// admins is the optional admin step-up authenticator (task #27 Phase B, REQ-522). nil = the admin
	// tier does not exist: elevate/admin-class routes fail closed; every other lane is unaffected.
	admins *AdminAuthenticator
}

var (
	ErrUnauthenticated = errors.New("auth: unauthenticated")
	ErrReplay          = errors.New("auth: nonce replay")
	ErrStale           = errors.New("auth: timestamp outside window")
	// ErrReplayProtectionUnconfigured fails closed when the HMAC path is used without both a bounded
	// timestamp window and a nonce store — a replayable authenticated request is an auth bypass.
	ErrReplayProtectionUnconfigured = errors.New("auth: HMAC replay protection not configured (need timestamp window + nonce store)")
)

// NewVerifier constructs a verifier and validates at construction that HMAC replay protection is
// configured (a bounded window AND a nonce store), so a misconfiguration fails at boot, not silently
// at request time.
func NewVerifier(sources SourceResolver, nonces NonceStore, window time.Duration) (*Verifier, error) {
	if window <= 0 || nonces == nil {
		return nil, ErrReplayProtectionUnconfigured
	}
	return &Verifier{Sources: sources, Nonces: nonces, Window: window}, nil
}

// authenticate verifies the request and returns a Principal, reading (and restoring) the raw body
// for the HMAC signature. It returns an error for ANY failure — fail closed.
func (v *Verifier) authenticate(r *http.Request) (Principal, error) {
	// mTLS path: a verified client certificate is sufficient.
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 && len(r.TLS.VerifiedChains) > 0 {
		cn := r.TLS.PeerCertificates[0].Subject.CommonName
		if cn == "" {
			return Principal{}, ErrUnauthenticated
		}
		// The CN encodes the source identity in deployment; kept minimal here.
		return Principal{SourceID: cn, Method: AuthMTLS}, nil
	}

	// HMAC path. Replay protection is MANDATORY here: both a bounded timestamp window AND a nonce
	// store must be configured, or we fail closed (defence against a misconfigured Verifier).
	if v.Window <= 0 || v.Nonces == nil {
		return Principal{}, ErrReplayProtectionUnconfigured
	}
	sourceID := r.Header.Get("X-TG-Source")
	tsStr := r.Header.Get("X-TG-Timestamp")
	nonce := r.Header.Get("X-TG-Nonce")
	sigHex := r.Header.Get("X-TG-Signature")
	if sourceID == "" || tsStr == "" || nonce == "" || sigHex == "" {
		return Principal{}, ErrUnauthenticated
	}
	tsUnix, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	ts := time.Unix(tsUnix, 0)
	if absDur(time.Since(ts)) > v.Window { // window guaranteed > 0 above
		return Principal{}, ErrStale
	}
	src, err := v.Sources.LookupSource(r.Context(), sourceID)
	if err != nil || len(src.HMACSecret) == 0 {
		// A source without an HMAC secret (e.g. a bearer-only push source under 0008) has NO HMAC path:
		// hmac.New with an empty key still computes, so without this guard a token-only source would be
		// forgeable by signing with the empty key. Fail closed instead.
		return Principal{}, ErrUnauthenticated
	}
	// Read the raw body for signing, then restore it for the (post-auth) handler. An oversized body
	// is rejected explicitly rather than signing a silently-truncated prefix.
	const maxBody = 8 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return Principal{}, ErrUnauthenticated
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	mac := hmac.New(sha256.New, src.HMACSecret)
	mac.Write([]byte(tsStr))
	mac.Write([]byte("\n"))
	mac.Write([]byte(nonce))
	mac.Write([]byte("\n"))
	mac.Write(body)
	want := mac.Sum(nil)
	got, err := hex.DecodeString(sigHex)
	if err != nil || subtle.ConstantTimeCompare(want, got) != 1 {
		return Principal{}, ErrUnauthenticated
	}
	seen, err := v.Nonces.SeenBefore(r.Context(), sourceID, nonce, ts) // nonce store guaranteed non-nil above
	if err != nil || seen {
		return Principal{}, ErrReplay
	}
	return Principal{SourceID: sourceID, Method: AuthHMAC}, nil
}

// authenticateIngestPush admits a per-source STATIC bearer token on an ingest route, for push sources that
// cannot compute an HMAC over the body (e.g. Alertmanager webhooks). It is fail-closed and constant-time,
// keys the source on the {source_type} URL slug, and is consulted by an AuthIngestPush route ONLY AFTER the
// stronger HMAC/mTLS path declines — so it never weakens those. A static token has NO replay protection; an
// AuthIngestPush route must be TLS-fronted in production.
func (v *Verifier) authenticateIngestPush(r *http.Request) (Principal, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return Principal{}, ErrUnauthenticated
	}
	sourceID := chi.URLParam(r, "source_type")
	if sourceID == "" {
		return Principal{}, ErrUnauthenticated
	}
	src, err := v.Sources.LookupSource(r.Context(), sourceID)
	if err != nil || len(src.IngestToken) == 0 {
		return Principal{}, ErrUnauthenticated // unknown source or no token provisioned ⇒ fail closed
	}
	// Compare fixed-length digests so the comparison is constant-time regardless of token length
	// (ConstantTimeCompare short-circuits on a length mismatch, which would leak the token's length).
	gotSum, wantSum := sha256.Sum256([]byte(token)), sha256.Sum256(src.IngestToken)
	if subtle.ConstantTimeCompare(gotSum[:], wantSum[:]) != 1 {
		return Principal{}, ErrUnauthenticated
	}
	return Principal{SourceID: sourceID, Method: AuthIngestPush}, nil
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// PrincipalHandler is a handler that REQUIRES a verified principal. The type makes it impossible to
// serve a route without authentication having produced a Principal.
type PrincipalHandler func(w http.ResponseWriter, r *http.Request, p Principal)

// RouteMethod is the HTTP method (GET/POST/…) a registered route actually serves, paired with its path
// pattern. It is the truth the handler enforces internally (its own `r.Method` guard), declared once at
// registration so a contract generator can emit only the real verb(s) per route instead of chi's
// all-method catch-all. It carries no auth material — it is purely the route↔method map.
type RouteMethod struct {
	Pattern string
	Method  string
}

// Router wraps a chi router and enforces that every registered route declares an auth method.
type Router struct {
	mux *chi.Mux
	v   *Verifier
	// declared records the HTTP method(s) each route serves, in registration order. It drives ONLY the
	// generated contract (gencontracts) — it is never consulted at serve time, so it cannot affect
	// dispatch or the auth model. Populated from the httpMethods a caller passes to Handle.
	declared []RouteMethod
}

// NewRouter builds a router bound to a verifier.
func NewRouter(v *Verifier) *Router { return &Router{mux: chi.NewRouter(), v: v} }

// Handle registers a route. It PANICS if method == AuthNone — a route with no auth cannot exist.
// The handler is invoked only after the caller is authenticated, with the resulting Principal.
//
// httpMethods declares the HTTP verb(s) the route actually serves (its handler's own `r.Method` truth).
// It does NOT change dispatch: the route is still registered for ALL methods via chi, so the auth
// middleware runs first and the handler enforces its own method (a session POST to a read route is still
// the auth layer's 403 "read-only", an unauthenticated request is still 401 — reject-before-parse, INV-01
// unchanged). The declaration is recorded only so the generated wire contract lists the real verb per
// route rather than every method. Omitting it (legacy/test callers) records nothing.
func (rt *Router) Handle(pattern string, method AuthMethod, h PrincipalHandler, httpMethods ...string) {
	if method == AuthNone {
		panic(fmt.Sprintf("auth: route %q registered with auth=none — refusing to create an open endpoint (INV-01)", pattern))
	}
	rt.mux.Handle(pattern, rt.wrap(method, h))
	for _, hm := range httpMethods {
		rt.declared = append(rt.declared, RouteMethod{Pattern: pattern, Method: hm})
	}
}

// DeclaredRoutes returns the HTTP method(s) declared for the registered routes, in registration order.
// A contract generator reads this to emit a truthful OpenAPI (one verb per route), while the served route
// table (Mux) still matches every method so the auth model is unchanged. A route registered without a
// declared method is absent here — the generator treats a served-but-undeclared path as a coverage gap.
func (rt *Router) DeclaredRoutes() []RouteMethod { return rt.declared }

func (rt *Router) wrap(method AuthMethod, h PrincipalHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p Principal
		switch method {
		case AuthOperatorLogin:
			// The login route: authentication IS the credential check + session mint. The handler only
			// sets the cookie exposed via context — it never sees an unauthenticated request (INV-01).
			principal, cookie, _, err := rt.v.authenticateOperatorLogin(r)
			if err != nil {
				if errors.Is(err, ErrLoginRateLimited) {
					http.Error(w, "rate limited", http.StatusTooManyRequests)
					return
				}
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), ctxKey{}, principal)
			ctx = context.WithValue(ctx, sessionCookieCtxKey{}, cookie)
			h(w, r.WithContext(ctx), principal)
			return
		case AuthSession:
			// Session-only route (logout): a valid cookie is the sole admissible credential.
			principal, err := rt.v.authenticateSession(r)
			if err != nil {
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			p = principal
		case AuthAdminElevate:
			// Step-up re-auth (task #27 Phase B): a VALID session cookie plus the separate admin
			// credential, verified before the handler runs. Success marks the server-side session
			// admin-elevated; the handler only reports the grant.
			principal, until, err := rt.v.authenticateAdminElevate(r)
			if err != nil {
				if errors.Is(err, ErrLoginRateLimited) {
					http.Error(w, "rate limited", http.StatusTooManyRequests)
					return
				}
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), ctxKey{}, principal)
			ctx = context.WithValue(ctx, adminGrantCtxKey{}, until)
			h(w, r.WithContext(ctx), principal)
			return
		case AuthAdminSession:
			// The admin write tier (REQ-522): only a valid session with a live admin elevation is
			// admitted; the principal carries Admin=true. Absent, expired, plain-session, and machine
			// callers are one indistinguishable 401 — this class never inspects HMAC/mTLS material.
			principal, err := rt.v.authenticateAdminSession(r)
			if err != nil {
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			p = principal
		case AuthReadOnly:
			// Machine callers (HMAC/mTLS) satisfy a read route exactly as they satisfy AuthHMAC —
			// tried FIRST and unchanged. Only if no machine credential is present is a session cookie
			// considered, and a session principal is admitted for safe (GET) requests ONLY.
			principal, err := rt.v.authenticate(r)
			if err != nil || principal.Method == AuthNone {
				principal, err = rt.v.authenticateSession(r)
				if err != nil {
					http.Error(w, "unauthenticated", http.StatusUnauthorized)
					return
				}
				if r.Method != http.MethodGet {
					http.Error(w, "session principals are read-only", http.StatusForbidden)
					return
				}
			}
			p = principal
		case AuthIngestPush:
			// The ingest front door. A machine caller (HMAC/mTLS) is tried FIRST and unchanged; the
			// per-source static bearer token is considered ONLY when no machine credential is PRESENT
			// (REQ-519) — a present-but-INVALID HMAC/mTLS credential is an active credential failure and
			// rejects outright, never falling through to a weaker scheme. The bearer path never weakens
			// HMAC/mTLS.
			principal, err := rt.v.authenticate(r)
			if err != nil || principal.Method == AuthNone {
				if r.Header.Get("X-TG-Signature") != "" || r.Header.Get("X-TG-Source") != "" ||
					(r.TLS != nil && len(r.TLS.PeerCertificates) > 0) {
					http.Error(w, "unauthenticated", http.StatusUnauthorized)
					return
				}
				principal, err = rt.v.authenticateIngestPush(r)
				if err != nil {
					http.Error(w, "unauthenticated", http.StatusUnauthorized)
					return
				}
			}
			p = principal
		case AuthTraceRead:
			// The elevated decision-tracer surface (spec/020 REQ-2014). Machine callers (HMAC/mTLS) satisfy it
			// as trusted system principals — tried FIRST and unchanged. Only if no machine credential is present
			// is a session considered, and ONLY an ADMIN-ELIGIBLE session (the distinct trace-read role) is
			// admitted, for safe (GET) requests. A plain read-only operator session — which satisfies
			// AuthReadOnly — is REFUSED (403), so this surface is strictly more elevated than the read-only console.
			principal, err := rt.v.authenticate(r)
			if err != nil || principal.Method == AuthNone {
				principal, err = rt.v.authenticateTraceReadSession(r)
				if err != nil {
					if errors.Is(err, ErrTraceReadRequired) {
						http.Error(w, "trace-read role required", http.StatusForbidden)
						return
					}
					http.Error(w, "unauthenticated", http.StatusUnauthorized)
					return
				}
				if r.Method != http.MethodGet {
					http.Error(w, "session principals are read-only", http.StatusForbidden)
					return
				}
			}
			p = principal
		default:
			principal, err := rt.v.authenticate(r)
			if err != nil || principal.Method == AuthNone {
				http.Error(w, "unauthenticated", http.StatusUnauthorized) // rejected BEFORE the handler parses the body
				return
			}
			// An AuthMTLS route requires an mTLS principal. An AuthHMAC route also accepts a stronger mTLS
			// principal by design — stronger auth may satisfy a weaker requirement, never the reverse.
			// A session principal can never appear here: authenticate() never reads cookies.
			if method == AuthMTLS && principal.Method != AuthMTLS {
				http.Error(w, "mTLS required", http.StatusUnauthorized)
				return
			}
			p = principal
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, p)
		h(w, r.WithContext(ctx), p)
	}
}

// Mux exposes the underlying handler for serving.
func (rt *Router) Mux() http.Handler { return rt.mux }
