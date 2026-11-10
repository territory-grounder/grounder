package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
)

// TG-294 — THE TIER FOR /v1/credentials/onboarding, PINNED BY A TEST BECAUSE IT ALREADY DRIFTED ONCE.
//
// The route shipped under auth.AuthReadOnly, the same tier as the whole read-only console, on the argument
// that its body carries no key material (references only, INV-13). What that argument missed is that the
// value of this surface is the RANKING, not the fields: migration 0054 stores `hosts integer -- blast
// radius` next to `mapped boolean` and indexes them as (mapped, hosts DESC), so a single GET returns the
// estate's unprotected credentials already sorted by how many hosts each one owns. Measured on this estate
// 2026-08-04, that is a named, ordered list of ten unmapped credentials — target selection handed to any
// principal that could read the console, while the agent's OWN evidence walk (/v1/sessions/{ref}, the
// tracer) required the strictly higher AuthTraceRead.
//
// An authz level with no test is a comment: it survives exactly until the next refactor moves the line.
// This test is the assertion, and it drives the REAL auth.Router through the REAL httpapi.Register — a
// handler-level test cannot see a tier at all, because the tier lives in the route table.
//
// KILLING MUTATION (executed): change router.go back to auth.AuthReadOnly on this route. RED —
// "a PLAIN read-only operator session was served the credential-onboarding map: got 200, want 403 ...
// any console-read operator can pull the ranked list of unprotected credentials by blast radius".
// Restored ⇒ green.
func TestCredentialOnboardingRequiresElevatedTraceReadTier(t *testing.T) {
	const (
		elevated = "/v1/credentials/onboarding" // the route under test (must be AuthTraceRead or higher)
		control  = "/v1/credentials/sources"    // a sibling that is legitimately AuthReadOnly
	)

	store := &roleGrantingSessionStore{MemSessionStore: auth.NewMemSessionStore()}
	ops := auth.MemOperators{"alice": {Name: "alice", TokenSHA256: sha256.Sum256([]byte("t0ken"))}}
	sa, err := auth.NewSessionAuthenticator([]byte(strings.Repeat("k", 32)), store, ops, time.Hour)
	if err != nil {
		t.Fatalf("session authenticator: %v", err)
	}
	sa.Secure = false // httptest serves plain HTTP; a Secure cookie would never be sent back
	v := &auth.Verifier{}
	v.EnableBrowserSessions(sa)

	rt := auth.NewRouter(v)
	Register(rt, Deps{Sessions: sa, Credentials: credentialsFixture(), CredentialOnboardingRead: staticOnboardingReader{}})

	// VACUITY FLOOR. Every assertion below reads a STATUS CODE, and a route that no longer exists answers
	// 404 for every caller — which is not "refused", it is "not asked". If the pattern is renamed, moved
	// behind a conditional, or deleted, this test must say so in those words rather than quietly grading a
	// 404 against a 403 it happens not to equal. Registration is the only place the tier can be observed.
	found := map[string]bool{elevated: false, control: false}
	for _, dr := range rt.DeclaredRoutes() {
		if _, watched := found[dr.Pattern]; watched && dr.Method == http.MethodGet {
			found[dr.Pattern] = true
		}
	}
	for pattern, seen := range found {
		if !seen {
			t.Fatalf("VACUITY FLOOR: no GET %s is registered by httpapi.Register — this test would then be "+
				"asserting an authz tier for a route that does not exist, and would pass by matching nothing. "+
				"If the route moved, move this assertion with it; if it was deleted, delete this test. "+
				"Declared routes: %s", pattern, declaredPatterns(rt))
		}
	}

	srv := httptest.NewServer(rt.Mux())
	defer srv.Close()

	// A PLAIN read-only operator session: authenticates to the console, holds no elevated role.
	cookie, _, err := sa.Login(context.Background(), "alice", "t0ken", "192.0.2.1:1234")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	// 1. THE REFUSAL. The read-only console tier must NOT reach the blast-radius map.
	code, body := authzGet(t, srv.URL+elevated, cookie)
	if code != http.StatusForbidden {
		t.Fatalf("a PLAIN read-only operator session was served the credential-onboarding map: got %d, want 403 "+
			"(TG-294 — this route serves migration 0054's (mapped, hosts DESC) ranking, so at AuthReadOnly any "+
			"console-read operator can pull the ranked list of unprotected credentials by blast radius, which is "+
			"target selection; it must sit at AuthTraceRead or higher, like the agent's own evidence walk)", code)
	}
	// ...and refused BY THE TRACE-READ GATE specifically. A 403 from some other guard (a method check, a
	// read-only-session POST refusal) would be the right number for the wrong reason, and would keep passing
	// if the tier itself were lowered again.
	if !strings.Contains(body, "trace-read") {
		t.Fatalf("403 body = %q, want the trace-read refusal — a 403 from a different guard would still pass "+
			"this test while the tier drifted back to read-only", strings.TrimSpace(body))
	}

	// 2. THE CONTROL. The SAME cookie is admitted to a sibling AuthReadOnly credentials read, proving the
	// refusal above is the TIER and not a broken/expired session, which would refuse everything equally.
	if code, _ := authzGet(t, srv.URL+control, cookie); code != http.StatusOK {
		t.Fatalf("the read-only console tier was refused at %s (%d) — the session itself is not valid, so the "+
			"403 above proves nothing about the onboarding route's tier", control, code)
	}

	// 3. THE ADMIT SIDE. The same session, once it holds admin standing (LDAP tg-admins, the standing
	// AuthTraceRead recognises), IS served the map. Without this the route could be raised to a tier no
	// human read caller can ever satisfy — "secure" by being dead, this repo's most expensive failure mode.
	id, _, _ := strings.Cut(cookie.Value, ".")
	store.grant(id)
	code, body = authzGet(t, srv.URL+elevated, cookie)
	if code != http.StatusOK {
		t.Fatalf("an ADMIN-ELIGIBLE session was refused the credential-onboarding map: got %d, want 200 — the "+
			"tier must be reachable by the operator who has to act on it (the write that resolves this list, "+
			"POST /v1/modules/{surface}/{source}/secret, is AuthAdminSession)", code)
	}
	// The 200 must be the REAL handler's body, not an empty or fail-closed one: a 503 would also mean "auth
	// let me past" while proving nothing served.
	var page CredentialOnboarding
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("elevated 200 body did not decode as CredentialOnboarding (%v): %q", err, strings.TrimSpace(body))
	}
	if page.Total != 2 || page.Unmapped != 1 {
		t.Fatalf("elevated read served total=%d unmapped=%d, want 2/1 — the fixture's map did not reach the "+
			"caller, so the 200 does not prove the elevated tier serves this surface", page.Total, page.Unmapped)
	}

	// 4. FAIL-CLOSED. No credential at all is 401 before the handler (INV-01) — never 200, never 404.
	if code, _ := authzGet(t, srv.URL+elevated, nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET %s: got %d, want 401", elevated, code)
	}
}

// authzGet issues a GET carrying the given session cookie (nil = unauthenticated) and returns status + body.
func authzGet(t *testing.T, url string, cookie *http.Cookie) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// declaredPatterns renders the registered route table for the vacuity floor's failure message: a floor that
// says only "not found" sends the reader back to the router to work out what it IS called.
func declaredPatterns(rt *auth.Router) string {
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	for _, dr := range rt.DeclaredRoutes() {
		if !seen[dr.Pattern] {
			seen[dr.Pattern] = true
			out = append(out, dr.Pattern)
		}
	}
	return strings.Join(out, " ")
}

// roleGrantingSessionStore is a MemSessionStore that ALSO implements auth.RoleStore. That optional interface
// is the only way a test outside package auth can put a session into the admin-eligible state AuthTraceRead
// requires (markAdminEligible is unexported; SessionAuthenticator.AdminEligible consults the durable
// RoleStore first, which is exactly the seam the restart-loses-the-role fix added).
type roleGrantingSessionStore struct {
	*auth.MemSessionStore
	mu       sync.Mutex
	eligible map[string]bool
}

func (s *roleGrantingSessionStore) grant(id string) {
	if err := s.SetAdminEligible(context.Background(), id, true); err != nil {
		panic(err) // the in-memory store cannot fail; a panic here means the fake drifted from the interface
	}
}

func (s *roleGrantingSessionStore) SetAdminEligible(_ context.Context, id string, eligible bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eligible == nil {
		s.eligible = map[string]bool{}
	}
	s.eligible[id] = eligible
	return nil
}

func (s *roleGrantingSessionStore) AdminEligible(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eligible[id], nil
}

// staticOnboardingReader serves a two-row map with ONE unmapped credential, so a 200 can be distinguished
// from a fail-closed 503 or an empty body by its contents.
type staticOnboardingReader struct{}

func (staticOnboardingReader) CredentialOnboarding(context.Context, auth.Principal) (CredentialOnboarding, error) {
	return CredentialOnboarding{
		Bindings: []CredentialBindingDTO{
			{Source: "awx", Name: "dc1-machine", Scope: "Leiden", Via: "jt-patch", Hosts: 41, Mapped: false},
			{Source: "awx", Name: "onekey", Scope: "all", Hosts: 3, Mapped: true, Usable: true, Ref: "bao:secret/data/tg/onekey#key"},
		},
		Total: 2, Unmapped: 1, Sources: []string{"awx"},
	}, nil
}
