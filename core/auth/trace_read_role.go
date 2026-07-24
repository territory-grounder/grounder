// The distinct, elevated trace-read role (spec/020 T-020-11, REQ-2014). The decision tracer exposes a
// session's decision internals — the rules, rationale, confidence, per-gate verdicts and scrubbed agent
// steps — so it must NOT be visible to the whole read-only console surface. This gates the trace surface
// behind an elevated role SEPARATE from AuthReadOnly: a machine principal (HMAC/mTLS) satisfies it as a
// trusted system caller (handled in the router, tried first), and a browser SESSION satisfies it ONLY when
// its server-side session is admin-eligible — an LDAP tg-admins member, the SAME elevated tier the admin
// write step-up recognizes (REQ-522). A plain read-only operator session is REFUSED.
//
// Provenance: [O] INV-01 (mandatory auth, default-deny), spec/020 REQ-2014. It reuses the existing
// adminEligible signal (set at login for tg-admins members) — no new group or credential is invented.
package auth

import (
	"errors"
	"net/http"
)

// ErrTraceReadRequired is returned when a caller holds a VALID browser session that lacks the distinct,
// elevated trace-read role (REQ-2014): it authenticated for the read-only console surface (AuthReadOnly) but
// is not admin-eligible, so the decision-tracer surface refuses it. It is DISTINCT from ErrUnauthenticated
// (an absent/invalid credential) so the router answers "authenticated but not authorized for the trace
// surface" as a 403, not a 401.
var ErrTraceReadRequired = errors.New("auth: trace-read role required")

// authenticateTraceReadSession admits a browser SESSION to the elevated decision-tracer surface ONLY when its
// server-side session is admin-eligible (an LDAP tg-admins member). A plain read-only operator session — one
// that satisfies AuthReadOnly — is refused with ErrTraceReadRequired, so the tracer's decision internals are
// visible to elevated operators only. Fail-closed at every step: no session authenticator ⇒
// ErrSessionUnconfigured; an absent/invalid/expired/revoked cookie ⇒ ErrUnauthenticated; a valid-but-non-
// eligible session ⇒ ErrTraceReadRequired.
//
// Machine principals (HMAC/mTLS) are NOT handled here — the router tries them FIRST (they are strictly more
// privileged and satisfy the elevated surface as trusted system callers); this session path is consulted only
// when no machine credential is present.
func (v *Verifier) authenticateTraceReadSession(r *http.Request) (Principal, error) {
	if v.sessions == nil {
		return Principal{}, ErrSessionUnconfigured
	}
	id, operator, err := v.sessions.verifyWithID(r)
	if err != nil {
		return Principal{}, err
	}
	if !v.sessions.AdminEligible(id) {
		// A valid session, but it holds only the read-only console role, not the elevated trace-read role.
		return Principal{}, ErrTraceReadRequired
	}
	// The admitted principal is a session (read-only METHOD) marked Admin — it holds the elevated trace-read
	// role. The router still restricts it to GET: trace-read is a READ elevation, never a write grant.
	return Principal{SourceID: "operator:" + operator, Method: AuthSession, Admin: true}, nil
}
