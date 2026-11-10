package httpapi

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/auth"
)

// TG-669: the world-model review verbs and the earned op-class verbs are AUTHORITY changes (an adopt widens
// the actuation allowlist; a ratify grants TG an executable verb). They sat on auth.AuthSession, so any
// logged-in READ-ONLY operator could grant the estate. These tests pin the three routes to the admin tier the
// same way seal_rewrap_test.go pins the DEK rewrap: through the REAL router + authenticators, not a handler
// call — a route can drift onto a weaker tier only by failing here.
//
// KILLING MUTATION (executed 2026-11-07): registering the three routes under auth.AuthSession again turned
// every "plain session is refused" case below RED (the write reached the fake backend), and registering them
// outside the AdminSessions block turned the "absent without the admin tier" cases RED.

type fakeOpClassWriter struct{ calls []string }

func (f *fakeOpClassWriter) Ratify(_ context.Context, key string, _ opschema.OpClassSpec, _ int, _, approver string) (OpClassOutcome, error) {
	f.calls = append(f.calls, "ratify/"+key+"/"+approver)
	return OpClassOutcome{CandidateKey: key, OpClass: "x"}, nil
}
func (f *fakeOpClassWriter) Dismiss(_ context.Context, key, _, approver string) (OpClassOutcome, error) {
	f.calls = append(f.calls, "dismiss/"+key+"/"+approver)
	return OpClassOutcome{CandidateKey: key, OpClass: "x"}, nil
}
func (f *fakeOpClassWriter) Demote(_ context.Context, opClass, _, approver string) (OpClassOutcome, error) {
	f.calls = append(f.calls, "demote/"+opClass+"/"+approver)
	return OpClassOutcome{OpClass: opClass}, nil
}
func (f *fakeOpClassWriter) Revoke(_ context.Context, opClass, _, approver string) (OpClassOutcome, error) {
	f.calls = append(f.calls, "revoke/"+opClass+"/"+approver)
	return OpClassOutcome{OpClass: opClass}, nil
}
func (f *fakeOpClassWriter) ExportEmbed(_ context.Context, opClass, _, approver string) (OpClassOutcome, error) {
	f.calls = append(f.calls, "export-embed/"+opClass+"/"+approver)
	return OpClassOutcome{OpClass: opClass}, nil
}

// the three authority-changing write routes, each with a body the handler accepts past validation.
var tg669Routes = []struct{ name, path, body string }{
	{"manifest adopt", "/v1/manifest/entries/1/adopt", `{"rationale":"the appliance is a real target"}`},
	{"opclass dismiss", "/v1/opclass/candidates/reload-haproxy%40nl/dismiss", `{"rationale":"not a capability TG should hold"}`},
	{"opclass revoke", "/v1/opclass/classes/reload-haproxy/revoke", `{"rationale":"withdraw the grant"}`},
	{"opclass demote", "/v1/opclass/classes/reload-haproxy/demote", `{"rationale":"back to approve after a deviation"}`},
	{"opclass export-embed", "/v1/opclass/classes/reload-haproxy/export-embed", `{"rationale":"promote into the reviewed registry"}`},
}

func tg669Deps(mw *fakeManifestWriter, ow *fakeOpClassWriter) Deps {
	return Deps{ManifestWrite: mw, OpClassWrite: ow}
}

// A logged-in, NOT elevated operator session is refused on all three routes, and NOTHING reaches a backend.
func TestTG669AuthorityWritesRefuseAPlainOperatorSession(t *testing.T) {
	mw, ow := &fakeManifestWriter{}, &fakeOpClassWriter{}
	rt, c := adminSurfaceRig(t, tg669Deps(mw, ow), false)
	for _, r := range tg669Routes {
		rec := adminPostJSON(rt, c, r.path, r.body)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: a NON-elevated session got %d, want 401 — a read-only operator could grant the estate", r.name, rec.Code)
		}
	}
	if len(mw.calls) != 0 || len(ow.calls) != 0 {
		t.Fatalf("a backend ran for an unelevated caller: manifest=%v opclass=%v", mw.calls, ow.calls)
	}
}

// The SAME operator, after the admin step-up, is admitted — and the backend sees the server-derived principal.
func TestTG669AuthorityWritesAdmitAnElevatedSession(t *testing.T) {
	mw, ow := &fakeManifestWriter{}, &fakeOpClassWriter{}
	rt, c := adminSurfaceRig(t, tg669Deps(mw, ow), true)
	for _, r := range tg669Routes {
		rec := adminPostJSON(rt, c, r.path, r.body)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: an elevated admin session got %d (%s), want 200 — the owner path must keep working", r.name, rec.Code, rec.Body.String())
		}
	}
	if len(mw.calls) != 1 || !strings.HasSuffix(mw.calls[0], "/kyriakos") {
		t.Fatalf("manifest backend: want exactly one call by the authenticated operator, got %v", mw.calls)
	}
	if len(ow.calls) != 4 {
		t.Fatalf("opclass backend: want dismiss+revoke+demote+export-embed by the authenticated operator, got %v", ow.calls)
	}
	for _, c := range ow.calls {
		if !strings.HasSuffix(c, "/kyriakos") {
			t.Fatalf("opclass backend call %q does not carry the server-derived operator", c)
		}
	}
}

// Without an admin authenticator the routes do not EXIST (fail closed) — the config-write pattern. A browser
// session alone must not reach them, so a deployment that never configured the admin tier cannot have its
// allowlist widened from a plain login.
func TestTG669AuthorityWritesAreAbsentWithoutTheAdminTier(t *testing.T) {
	mw, ow := &fakeManifestWriter{}, &fakeOpClassWriter{}
	d := tg669Deps(mw, ow)
	v := &auth.Verifier{}
	sa, err := auth.NewSessionAuthenticator([]byte(strings.Repeat("k", 32)), auth.NewMemSessionStore(),
		auth.MemOperators{"kyriakos": {Name: "kyriakos", TokenSHA256: sha256.Sum256([]byte("op-token-test"))}}, time.Hour)
	if err != nil {
		t.Fatalf("session authenticator: %v", err)
	}
	sa.Secure = false
	v.EnableBrowserSessions(sa)
	d.Sessions = sa // and NO AdminSessions
	rt := auth.NewRouter(v)
	Register(rt, d)
	cookie, _, err := sa.Login(context.Background(), "kyriakos", "op-token-test", "192.0.2.9:1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	for _, r := range tg669Routes {
		rec := adminPostJSON(rt, cookie, r.path, r.body)
		// Exactly 404: the pattern is not registered at all. A 401 here would mean the route EXISTS and merely
		// refused this caller — a weaker posture than "no admin tier, no grant lane".
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d with NO admin tier configured, want 404 — the route must not exist", r.name, rec.Code)
		}
	}
	if len(mw.calls) != 0 || len(ow.calls) != 0 {
		t.Fatalf("a backend ran with no admin tier configured: manifest=%v opclass=%v", mw.calls, ow.calls)
	}
}
