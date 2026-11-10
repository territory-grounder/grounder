// The distinct, elevated trace-read role (spec/020 T-020-11, REQ-2014). The decision tracer exposes a
// session's decision internals — the rules, rationale, confidence, per-gate verdicts and scrubbed agent
// steps — so it must NOT be visible to the whole read-only console surface. This gates the trace surface
// behind an elevated role SEPARATE from AuthReadOnly: a machine principal (HMAC/mTLS) satisfies it as a
// trusted system caller (handled in the router, tried first), and a browser SESSION satisfies it when it
// holds ADMIN STANDING BY EITHER ROUTE — LDAP tg-admins membership proven at login, or a live admin
// step-up. A plain read-only operator session is REFUSED.
//
// BOTH ROUTES, because this deployment has two admin identities — one local (static credential) and one
// LDAP — and both are TG admins. Accepting only the LDAP-proven one produced an incoherent split: the
// local admin could WRITE config, store secrets and run module tests through the step-up, yet was refused
// a READ of the tracer. The write tier is strictly the more dangerous of the two, so refusing the read
// while permitting the writes protected nothing and simply made the page unusable for a real admin.
//
// Provenance: [O] INV-01 (mandatory auth, default-deny), spec/020 REQ-2014. It reuses the existing
// adminEligible signal (set at login for tg-admins members) — no new group or credential is invented.
package auth

import (
	"errors"
	"net/http"
)

// traceReadStanding reports whether a session holds admin standing by EITHER route. Split out so the two
// arms are named and separately testable — an unnamed `||` in the middle of an auth check is how one arm
// quietly stops being exercised.
func (v *Verifier) traceReadStanding(id string) bool {
	if v.sessions.AdminEligible(id) {
		return true // LDAP tg-admins, proven at login
	}
	if v.admins == nil {
		return false // no step-up configured ⇒ the LDAP arm is the only one
	}
	_, elevated := v.admins.Elevated(id)
	return elevated // a live admin step-up (the local admin credential satisfies this)
}

// ErrTraceReadRequired is returned when a caller holds a VALID browser session that lacks the distinct,
// elevated trace-read role (REQ-2014): it authenticated for the read-only console surface (AuthReadOnly) but
// is not admin-eligible, so the decision-tracer surface refuses it. It is DISTINCT from ErrUnauthenticated
// (an absent/invalid credential) so the router answers "authenticated but not authorized for the trace
// surface" as a 403, not a 401.
var ErrTraceReadRequired = errors.New("auth: trace-read role required")

// authenticateTraceReadSession admits a browser SESSION to the elevated decision-tracer surface when it
// holds admin standing by EITHER route: admin-eligible (LDAP tg-admins, proven at login) or currently
// admin-elevated (the step-up, which the local admin credential satisfies). A plain read-only operator
// session — one that satisfies AuthReadOnly — is refused with ErrTraceReadRequired. Fail-closed at every
// step: no session authenticator ⇒ ErrSessionUnconfigured; an absent/invalid/expired/revoked cookie ⇒
// ErrUnauthenticated; a valid session with neither standing ⇒ ErrTraceReadRequired.
//
// The step-up arm is TTL-BOUNDED by construction (AdminAuthenticator.Elevated expires and self-evicts), so
// this grants trace-read for the life of an elevation rather than the life of a session — strictly tighter
// than the LDAP arm it sits beside.
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
	if !v.traceReadStanding(id) {
		// A valid session, but it holds only the read-only console role, not the elevated trace-read role.
		return Principal{}, ErrTraceReadRequired
	}
	// The admitted principal is a session (read-only METHOD) marked Admin — it holds the elevated trace-read
	// role. The router still restricts it to GET: trace-read is a READ elevation, never a write grant.
	return Principal{SourceID: "operator:" + operator, Method: AuthSession, Admin: true}, nil
}
