package auth

// THE LOCAL ADMIN REACHES THE TRACER TOO.
//
// This deployment has two admin identities — one local (static credential) and one LDAP — and both are TG
// admins. The trace-read gate originally accepted ONLY the LDAP-proven one, which produced an incoherent
// split: the local admin could WRITE config, store secrets and run module tests through the step-up, yet
// was refused a READ of the decision tracer. The write tier is strictly the more dangerous of the two, so
// refusing the read while permitting the writes protected nothing and made the page unusable for a real
// admin. Found by opening the page: "Recorded sessions need an admin session", 403, on a session that had
// just elevated successfully.

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func traceRig(t *testing.T) (*Verifier, *SessionAuthenticator, *AdminAuthenticator, *httptest.Server) {
	t.Helper()
	store := NewMemSessionStore()
	sa, err := NewSessionAuthenticator([]byte("0123456789abcdef0123456789abcdef"), store,
		MemOperators{"alice": {Name: "alice", TokenSHA256: sha256.Sum256([]byte("t0ken"))}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sa.Secure = false
	aa, err := NewAdminAuthenticator(MemOperators{"root-admin": {Name: "root-admin", TokenSHA256: sha256.Sum256([]byte("adm1n"))}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	v := &Verifier{}
	v.EnableBrowserSessions(sa)
	v.EnableAdminSessions(aa)

	rt := NewRouter(v)
	ok := func(w http.ResponseWriter, _ *http.Request, _ Principal) { w.WriteHeader(http.StatusOK) }
	rt.Handle("/v1/sessions/{external_ref}", AuthTraceRead, ok, http.MethodGet)
	return v, sa, aa, httptest.NewServer(rt.Mux())
}

func traceGet(t *testing.T, srv *httptest.Server, cookie *http.Cookie) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions/ref-1", nil)
	req.AddCookie(cookie)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

// KILLING MUTATION: drop the step-up arm from traceReadStanding (accept only AdminEligible). RED — the
// local admin is locked out of the tracer while retaining every admin WRITE, which is the defect this
// closes.
func TestALocallyElevatedAdminReachesTheTracer(t *testing.T) {
	_, sa, aa, srv := traceRig(t)
	defer srv.Close()

	cookie, _, err := sa.Login(context.Background(), "alice", "t0ken", "192.0.2.1:1234")
	if err != nil {
		t.Fatal(err)
	}
	id := sessionIDOf(t, sa, cookie)

	// BEFORE the step-up: a plain operator session is refused. This is the control — without it the test
	// could pass on a gate that admits everyone.
	if got := traceGet(t, srv, cookie); got != http.StatusForbidden {
		t.Fatalf("a plain read-only session got %d, want 403 — the tracer must stay closed to the read-only "+
			"console surface", got)
	}

	// The LOCAL admin step-up — the static credential path, exactly what the console's elevate does.
	if _, err := aa.Elevate(context.Background(), id, "root-admin", "adm1n", "192.0.2.1:1234"); err != nil {
		t.Fatalf("local admin elevation failed: %v", err)
	}
	if got := traceGet(t, srv, cookie); got != http.StatusOK {
		t.Fatalf("an ELEVATED local admin got %d, want 200 — this account may write config and store "+
			"secrets through the same step-up; refusing it a READ protects nothing", got)
	}
}

// KILLING MUTATION: make the step-up arm ignore expiry (return true for any known id). RED — trace-read
// must end with the elevation, not outlive it.
func TestTraceReadEndsWithTheElevation(t *testing.T) {
	_, sa, aa, srv := traceRig(t)
	defer srv.Close()

	cookie, _, err := sa.Login(context.Background(), "alice", "t0ken", "192.0.2.1:1234")
	if err != nil {
		t.Fatal(err)
	}
	id := sessionIDOf(t, sa, cookie)

	now := time.Now()
	aa.WithClock(func() time.Time { return now })
	if _, err := aa.Elevate(context.Background(), id, "root-admin", "adm1n", "192.0.2.1:1234"); err != nil {
		t.Fatal(err)
	}
	if got := traceGet(t, srv, cookie); got != http.StatusOK {
		t.Fatalf("freshly elevated: got %d, want 200", got)
	}

	now = now.Add(2 * time.Hour) // past the 1h elevation TTL
	if got := traceGet(t, srv, cookie); got != http.StatusForbidden {
		t.Fatalf("after the elevation expired: got %d, want 403 — trace-read is granted for the life of an "+
			"elevation, never the life of a session", got)
	}
}

// KILLING MUTATION: let traceReadStanding panic or admit when no admin authenticator is wired. RED —
// a deployment with no step-up configured must fall back to the LDAP arm alone, never open up.
func TestNoAdminAuthenticatorMeansTheLDAPArmAlone(t *testing.T) {
	store := NewMemSessionStore()
	sa, err := NewSessionAuthenticator([]byte("0123456789abcdef0123456789abcdef"), store,
		MemOperators{"alice": {Name: "alice", TokenSHA256: sha256.Sum256([]byte("t0ken"))}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sa.Secure = false
	v := &Verifier{}
	v.EnableBrowserSessions(sa) // deliberately NO EnableAdminSessions

	rt := NewRouter(v)
	ok := func(w http.ResponseWriter, _ *http.Request, _ Principal) { w.WriteHeader(http.StatusOK) }
	rt.Handle("/v1/sessions/{external_ref}", AuthTraceRead, ok, http.MethodGet)
	srv := httptest.NewServer(rt.Mux())
	defer srv.Close()

	cookie, _, err := sa.Login(context.Background(), "alice", "t0ken", "192.0.2.1:1234")
	if err != nil {
		t.Fatal(err)
	}
	if got := traceGet(t, srv, cookie); got != http.StatusForbidden {
		t.Fatalf("with no admin authenticator wired, a plain session got %d, want 403", got)
	}
}

// sessionIDOf recovers the server-side session id the way the auth stack does — through verifyWithID on a
// request carrying the cookie — rather than by reaching into the cookie's encoding.
func sessionIDOf(t *testing.T, sa *SessionAuthenticator, cookie *http.Cookie) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://x/v1/sessions/ref-1", nil)
	req.AddCookie(cookie)
	id, _, err := sa.verifyWithID(req)
	if err != nil {
		t.Fatalf("verifyWithID: %v", err)
	}
	return id
}
