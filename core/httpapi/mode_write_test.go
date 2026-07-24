package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/policy"
)

// The owner's flip path: an admin-session operator posts a Shadow->Full-auto transition; the handler
// derives the operator + admin proof from the authenticated principal (never the body) and forwards the
// validated order to the worker backend.
func TestModeTransitionHappyPathUsesPrincipalIdentity(t *testing.T) {
	mt := &MemModeTransitioner{Outcome: ModeTransitionOutcome{Mode: "Full-auto", From: "Shadow", To: "Full-auto"}}
	rt, c := adminSurfaceRig(t, Deps{ModeTransition: mt}, true)
	rec := adminPostJSON(rt, c, "/v1/mode", `{"to":"Full-auto","expected_from":"Shadow","reason":"owner-present canary GO"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("mode flip: got %d (%s)", rec.Code, rec.Body.String())
	}
	var out ModeTransitionOutcome
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("outcome decode: %v", err)
	}
	if out.Mode != "Full-auto" || out.From != "Shadow" || out.To != "Full-auto" {
		t.Fatalf("outcome = %+v", out)
	}
	// The operator + admin proof MUST come from the server-authenticated principal.
	if mt.LastOperator != "kyriakos" {
		t.Fatalf("operator must be the authenticated session principal, got %q", mt.LastOperator)
	}
	if !mt.LastAdmin {
		t.Fatalf("admin-group signal must be forwarded (AuthAdminSession principal), got %v", mt.LastAdmin)
	}
	if mt.LastTo != "Full-auto" || mt.LastExpectedFrom != "Shadow" || mt.LastReason != "owner-present canary GO" {
		t.Fatalf("forwarded order = %+v", mt)
	}
}

// A denied flip (the worker's AuthorityChecker refused) surfaces as 403; a non-green preflight as 409.
func TestModeTransitionRefusalsMapToHonestStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"unauthorized", policy.ErrUnauthorizedModeChange, http.StatusForbidden},
		{"empty-actor", policy.ErrEmptyModeChangeActor, http.StatusForbidden},
		{"preflight-red", policy.ErrPreflightNotGreen, http.StatusConflict},
		{"stale-from", policy.ErrStaleMode, http.StatusConflict},
		{"unknown-mode", policy.ErrUnknownMode, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mt := &MemModeTransitioner{Err: tc.err}
			rt, c := adminSurfaceRig(t, Deps{ModeTransition: mt}, true)
			rec := adminPostJSON(rt, c, "/v1/mode", `{"to":"Full-auto","reason":"flip"}`)
			if rec.Code != tc.want {
				t.Fatalf("%s: got %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// Surface validation: unknown target mode, missing rationale — fail closed BEFORE the backend is called.
func TestModeTransitionSurfaceValidation(t *testing.T) {
	mt := &MemModeTransitioner{Outcome: ModeTransitionOutcome{Mode: "Shadow"}}
	rt, c := adminSurfaceRig(t, Deps{ModeTransition: mt}, true)

	if rec := adminPostJSON(rt, c, "/v1/mode", `{"to":"Turbo","reason":"r"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown target mode: got %d, want 400", rec.Code)
	}
	if rec := adminPostJSON(rt, c, "/v1/mode", `{"to":"Full-auto"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing rationale: got %d, want 400", rec.Code)
	}
	if rec := adminPostJSON(rt, c, "/v1/mode", `{"to":"","reason":"r"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty target: got %d, want 400", rec.Code)
	}
	if rec := adminPostJSON(rt, c, "/v1/mode", `{"to":"Full-auto","expected_from":"Turbo","reason":"r"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad expected_from: got %d, want 400", rec.Code)
	}
	if mt.Calls != 0 {
		t.Fatalf("a malformed request reached the backend %d time(s)", mt.Calls)
	}
}

// The surface is fail-closed and structurally admin-only: nil backend ⇒ 503; a plain (unelevated) session
// ⇒ 401; a cross-origin POST ⇒ 403; and with NO admin authenticator the route does not exist at all.
func TestModeTransitionFailClosedAndAdminOnly(t *testing.T) {
	// nil backend.
	rt, c := adminSurfaceRig(t, Deps{}, true)
	if rec := adminPostJSON(rt, c, "/v1/mode", `{"to":"Full-auto","reason":"r"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil backend: got %d, want 503", rec.Code)
	}
	// plain session, not elevated.
	mt := &MemModeTransitioner{}
	rt2, c2 := adminSurfaceRig(t, Deps{ModeTransition: mt}, false)
	if rec := adminPostJSON(rt2, c2, "/v1/mode", `{"to":"Full-auto","reason":"r"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("plain session on mode flip: got %d, want 401", rec.Code)
	}
	// cross-origin.
	rt3, c3 := adminSurfaceRig(t, Deps{ModeTransition: &MemModeTransitioner{}}, true)
	req := httptest.NewRequest(http.MethodPost, "/v1/mode", strings.NewReader(`{"to":"Full-auto","reason":"r"}`))
	req.AddCookie(c3)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	rt3.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin mode flip: got %d, want 403", rec.Code)
	}
	if mt.Calls != 0 {
		t.Fatalf("an unauthorized/unelevated request reached the backend")
	}
}
