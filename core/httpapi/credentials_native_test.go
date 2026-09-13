package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
)

const nativeTestEntry = "host-glob:web-*|deploy|22|ssh|env:TG_TEST_KEY_A"

// adminDeleteJSON issues a DELETE with a JSON body through the admin rig, mirroring adminPostJSON.
func adminDeleteJSON(rt *auth.Router, c *http.Cookie, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodDelete, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	rt.Mux().ServeHTTP(rec, req)
	return rec
}

// A valid add flows the VALIDATED entry + rationale and the SERVER-derived operator/admin proof to the
// writer, and returns the worker's outcome.
func TestNativeRuleAddUsesSessionIdentity(t *testing.T) {
	mw := &MemNativeRuleWriter{Outcome: NativeRuleWriteOutcome{ID: 7, LedgerSeq: 41}}
	rt, c := adminSurfaceRig(t, Deps{NativeRuleWrite: mw}, true)
	rec := adminPostJSON(rt, c, "/v1/credentials/native/rules",
		`{"entry":"`+nativeTestEntry+`","rationale":"cover the web tier"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add: got %d (%s)", rec.Code, rec.Body.String())
	}
	var out NativeRuleWriteOutcome
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("outcome decode: %v", err)
	}
	if out.ID != 7 || out.LedgerSeq != 41 {
		t.Fatalf("outcome = %+v", out)
	}
	if mw.LastVerb != "add" || mw.LastEntry != nativeTestEntry || mw.LastRationale != "cover the web tier" {
		t.Fatalf("writer call = %+v", mw)
	}
	if mw.LastOperator != "kyriakos" || !mw.LastAdmin {
		t.Fatalf("operator identity/admin proof must be the SERVER-authenticated principal's, got %q admin=%v", mw.LastOperator, mw.LastAdmin)
	}
}

// The surface's fast validation refuses BEFORE the worker: a malformed entry 400s with the validator's
// text verbatim, a multi-rule entry 400s with the one-row-one-rule refusal, and a missing rationale 400s —
// the writer is never called for any of them.
func TestNativeRuleAddSurfaceValidation(t *testing.T) {
	mw := &MemNativeRuleWriter{}
	rt, c := adminSurfaceRig(t, Deps{NativeRuleWrite: mw}, true)

	rec := adminPostJSON(rt, c, "/v1/credentials/native/rules", `{"entry":"host:broken|root","rationale":"r"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "need at least kind:pattern") {
		t.Fatalf("malformed entry: got %d %q, want 400 carrying the ParseRules text verbatim", rec.Code, rec.Body.String())
	}
	rec = adminPostJSON(rt, c, "/v1/credentials/native/rules",
		`{"entry":"host:a|root|22|ssh|env:K; host:b|root|22|ssh|env:K","rationale":"r"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "one row, one rule") {
		t.Fatalf("two-rule entry: got %d %q, want the one-row-one-rule 400", rec.Code, rec.Body.String())
	}
	if rec := adminPostJSON(rt, c, "/v1/credentials/native/rules", `{"entry":"`+nativeTestEntry+`"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing rationale: got %d, want 400", rec.Code)
	}
	if rec := adminPostJSON(rt, c, "/v1/credentials/native/rules", `{"rationale":"r"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing entry: got %d, want 400", rec.Code)
	}
	if mw.Calls != 0 {
		t.Fatalf("a surface-refused add reached the writer %d time(s)", mw.Calls)
	}
}

// Nil backend ⇒ 503 (fail closed); a plain non-elevated session ⇒ 401 (the admin tier is the gate).
func TestNativeRuleWriteFailClosed(t *testing.T) {
	rt, c := adminSurfaceRig(t, Deps{}, true)
	if rec := adminPostJSON(rt, c, "/v1/credentials/native/rules", `{"entry":"`+nativeTestEntry+`","rationale":"r"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil backend: got %d, want 503", rec.Code)
	}

	mw := &MemNativeRuleWriter{}
	rt, c = adminSurfaceRig(t, Deps{NativeRuleWrite: mw}, false) // logged in, NOT elevated
	if rec := adminPostJSON(rt, c, "/v1/credentials/native/rules", `{"entry":"`+nativeTestEntry+`","rationale":"r"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("plain session on native-rule write: got %d, want 401", rec.Code)
	}
	if mw.Calls != 0 {
		t.Fatalf("an unauthorized add reached the writer")
	}
}

// A delete flows the id + rationale + principal to the writer; a worker not-found maps to 404; a bad id
// or a missing rationale 400s before the writer.
func TestNativeRuleDelete(t *testing.T) {
	mw := &MemNativeRuleWriter{Outcome: NativeRuleWriteOutcome{ID: 5, LedgerSeq: 42}}
	rt, c := adminSurfaceRig(t, Deps{NativeRuleWrite: mw}, true)

	rec := adminDeleteJSON(rt, c, "/v1/credentials/native/rules/5", `{"rationale":"retired host"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: got %d (%s)", rec.Code, rec.Body.String())
	}
	if mw.LastVerb != "delete" || mw.LastID != 5 || mw.LastRationale != "retired host" || mw.LastOperator != "kyriakos" || !mw.LastAdmin {
		t.Fatalf("writer call = %+v", mw)
	}

	mw.Err = ErrNoSuchNativeRule
	if rec := adminDeleteJSON(rt, c, "/v1/credentials/native/rules/99", `{"rationale":"r"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing: got %d, want 404", rec.Code)
	}

	mw.Err = nil
	calls := mw.Calls
	if rec := adminDeleteJSON(rt, c, "/v1/credentials/native/rules/abc", `{"rationale":"r"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-numeric id: got %d, want 400", rec.Code)
	}
	if rec := adminDeleteJSON(rt, c, "/v1/credentials/native/rules/5", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing rationale: got %d, want 400", rec.Code)
	}
	if mw.Calls != calls {
		t.Fatalf("a surface-refused delete reached the writer")
	}
}

// TG-109 — THE TIER FOR GET /v1/credentials/native, PINNED LIKE THE ONBOARDING TIER (TG-294): the rows
// pair a target selector with the SecretRef string that unlocks it, so a plain read-only console session
// is REFUSED (403, by the trace-read gate), an admin-eligible session is served the real rows, an
// unauthenticated caller is 401, and a nil backend is an honest 503 — never an empty 200.
func TestNativeRulesReadTierAndHappyPath(t *testing.T) {
	store := &roleGrantingSessionStore{MemSessionStore: auth.NewMemSessionStore()}
	ops := auth.MemOperators{"alice": {Name: "alice", TokenSHA256: sha256.Sum256([]byte("t0ken"))}}
	sa, err := auth.NewSessionAuthenticator([]byte(strings.Repeat("k", 32)), store, ops, time.Hour)
	if err != nil {
		t.Fatalf("session authenticator: %v", err)
	}
	sa.Secure = false
	v := &auth.Verifier{}
	v.EnableBrowserSessions(sa)
	rt := auth.NewRouter(v)
	mr := &MemNativeRulesReader{Rules: []NativeRule{
		{ID: 1, Entry: nativeTestEntry, Rationale: "cover the web tier", CreatedBy: "operator:kyriakosp", CreatedAt: "2026-08-13T10:00:00Z"},
		{ID: 2, Entry: "host:db01|postgres|22|ssh|store:db01.key", Rationale: "db01 maintenance", CreatedBy: "operator:kyriakosp", CreatedAt: "2026-08-13T10:01:00Z"},
	}}
	Register(rt, Deps{Sessions: sa, CredentialNativeRead: mr})

	// Vacuity floor: the route must exist or every assertion below grades a 404.
	seen := false
	for _, dr := range rt.DeclaredRoutes() {
		if dr.Pattern == "/v1/credentials/native" && dr.Method == http.MethodGet {
			seen = true
		}
	}
	if !seen {
		t.Fatal("VACUITY FLOOR: GET /v1/credentials/native is not registered — this test would grade a 404")
	}

	srv := httptest.NewServer(rt.Mux())
	defer srv.Close()
	cookie, _, err := sa.Login(context.Background(), "alice", "t0ken", "192.0.2.1:1234")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	// A PLAIN read-only session is refused BY THE TRACE-READ GATE (the rows map credentials to targets).
	code, body := authzGet(t, srv.URL+"/v1/credentials/native", cookie)
	if code != http.StatusForbidden || !strings.Contains(body, "trace-read") {
		t.Fatalf("plain session: got %d %q, want the 403 trace-read refusal", code, strings.TrimSpace(body))
	}

	// An admin-eligible session is served the REAL rows.
	id, _, _ := strings.Cut(cookie.Value, ".")
	store.grant(id)
	code, body = authzGet(t, srv.URL+"/v1/credentials/native", cookie)
	if code != http.StatusOK {
		t.Fatalf("admin-eligible read: got %d (%s)", code, body)
	}
	var page NativeRulesPage
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("page decode: %v (%s)", err, body)
	}
	if page.Total != 2 || len(page.Rules) != 2 || page.Rules[0].Entry != nativeTestEntry {
		t.Fatalf("page = %+v, want the two fixture rows", page)
	}

	// Unauthenticated ⇒ 401 before the handler (INV-01).
	if code, _ := authzGet(t, srv.URL+"/v1/credentials/native", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: got %d, want 401", code)
	}
}

// Nil reader ⇒ 503 fail-closed, never an empty 200 that reads as "no rules configured".
func TestNativeRulesReadNilBackendIs503(t *testing.T) {
	store := &roleGrantingSessionStore{MemSessionStore: auth.NewMemSessionStore()}
	ops := auth.MemOperators{"alice": {Name: "alice", TokenSHA256: sha256.Sum256([]byte("t0ken"))}}
	sa, err := auth.NewSessionAuthenticator([]byte(strings.Repeat("k", 32)), store, ops, time.Hour)
	if err != nil {
		t.Fatalf("session authenticator: %v", err)
	}
	sa.Secure = false
	v := &auth.Verifier{}
	v.EnableBrowserSessions(sa)
	rt := auth.NewRouter(v)
	Register(rt, Deps{Sessions: sa})

	srv := httptest.NewServer(rt.Mux())
	defer srv.Close()
	cookie, _, err := sa.Login(context.Background(), "alice", "t0ken", "192.0.2.1:1234")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	id, _, _ := strings.Cut(cookie.Value, ".")
	store.grant(id)
	if code, _ := authzGet(t, srv.URL+"/v1/credentials/native", cookie); code != http.StatusServiceUnavailable {
		t.Fatalf("nil backend: got %d, want 503", code)
	}
}
