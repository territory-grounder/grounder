package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
)

// Browser operator sessions (spec/006 REQ-508): the login/logout surface the operator console uses.
// Authentication itself lives in core/auth — the login handler runs only AFTER the operator's bearer
// token was verified and the session minted (auth.AuthOperatorLogin), and the logout handler only
// after a valid session cookie authenticated the caller (auth.AuthSession). These handlers therefore
// never see an unauthenticated request (INV-01), and the session principal they serve is read-only by
// construction — it can only ever reach AuthReadOnly GET surfaces.

// SessionGrant is the login response: who the session is for and when it lapses. The credential itself
// travels ONLY in the HttpOnly cookie — never in the JSON body.
type SessionGrant struct {
	Operator  string    `json:"operator"`
	ExpiresAt time.Time `json:"expires_at"`
}

// sessionLoginHandler completes a verified operator login: it sets the minted HttpOnly session cookie
// and reports the grant. POST-only — a login is an action, not a read.
func (d Deps) sessionLoginHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cookie, ok := auth.PendingSessionCookie(r.Context())
	if !ok || cookie == nil {
		// Structurally unreachable (the login route always mints a cookie) — fail closed regardless.
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, cookie)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(SessionGrant{Operator: p.SourceID, ExpiresAt: cookie.Expires})
}

// AdminGrant is the elevation response (task #27 Phase B, REQ-522): who elevated and until when.
// The elevation itself is SERVER-SIDE state on the session — no new cookie, no bearer in the body.
type AdminGrant struct {
	Operator   string    `json:"operator"`
	AdminUntil time.Time `json:"admin_until"`
}

// sessionElevateHandler completes a verified admin step-up (auth.AuthAdminElevate verified the
// session cookie AND the separate admin credential, and marked the session elevated before this
// handler ran). It only reports the grant. POST-only; same-origin like every write-lane surface.
func (d Deps) sessionElevateHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin elevation rejected", http.StatusForbidden)
		return
	}
	until, ok := auth.PendingAdminGrant(r.Context())
	if !ok {
		// Structurally unreachable (the elevate route always mints a grant) — fail closed regardless.
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(AdminGrant{Operator: p.SourceID, AdminUntil: until})
}

// sessionLogoutHandler revokes the caller's own session (the cookie was verified by the router; the
// revocation targets the id inside it, so a caller can only ever revoke itself). POST-only.
func (d Deps) sessionLogoutHandler(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Sessions == nil {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := d.Sessions.RevokeRequest(r); err != nil {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	// Expire the cookie client-side too; the server-side revocation above is the authoritative one.
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}
