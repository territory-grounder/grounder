package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/policy"
)

// The admin toggle path: an admin-session operator posts an engine disable; the handler DERIVES the operator +
// admin proof from the authenticated principal (never the body) and forwards the validated order to the
// worker backend.
func TestEngineToggleHappyPathUsesPrincipalIdentity(t *testing.T) {
	et := &MemEngineToggler{Outcome: EngineToggleOutcome{Enabled: false, Mode: "Semi-auto", WarningCode: "engine-disabled"}}
	rt, c := adminSurfaceRig(t, Deps{EngineToggle: et}, true)
	rec := adminPostJSON(rt, c, "/v1/policy/engine-toggle", `{"enable":false,"reason":"maintenance window","double_confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("engine toggle: got %d (%s)", rec.Code, rec.Body.String())
	}
	var out EngineToggleOutcome
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("outcome decode: %v", err)
	}
	if out.Enabled != false || out.Mode != "Semi-auto" {
		t.Fatalf("outcome = %+v", out)
	}
	// The operator + admin proof MUST come from the server-authenticated principal, never the request body.
	if et.LastOperator != "kyriakos" {
		t.Fatalf("operator must be the authenticated session principal, got %q", et.LastOperator)
	}
	if !et.LastAdmin {
		t.Fatalf("admin-group signal must be forwarded (AuthAdminSession principal), got %v", et.LastAdmin)
	}
	if et.LastEnable != false || et.LastReason != "maintenance window" || !et.LastDoubleConfirm {
		t.Fatalf("forwarded order = %+v", et)
	}
}

// A denied toggle (the worker's AuthorityChecker refused) surfaces as 403; an unconfirmed one as 409.
func TestEngineToggleRefusalsMapToHonestStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"unauthorized", policy.ErrUnauthorizedEngineToggle, http.StatusForbidden},
		{"not-confirmed", policy.ErrEngineToggleNotConfirmed, http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			et := &MemEngineToggler{Err: tc.err}
			rt, c := adminSurfaceRig(t, Deps{EngineToggle: et}, true)
			rec := adminPostJSON(rt, c, "/v1/policy/engine-toggle", `{"enable":false,"reason":"r","double_confirm":true}`)
			if rec.Code != tc.want {
				t.Fatalf("%s: got %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// Surface validation: a missing rationale fails closed BEFORE the backend is called (the reason doubles as the
// audited acknowledgement text, so a blank one could never confirm).
func TestEngineToggleSurfaceValidation(t *testing.T) {
	et := &MemEngineToggler{Outcome: EngineToggleOutcome{Enabled: true}}
	rt, c := adminSurfaceRig(t, Deps{EngineToggle: et}, true)
	if rec := adminPostJSON(rt, c, "/v1/policy/engine-toggle", `{"enable":true}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing rationale: got %d, want 400", rec.Code)
	}
	if rec := adminPostJSON(rt, c, "/v1/policy/engine-toggle", `{"enable":true,"reason":"  "}`); rec.Code != http.StatusBadRequest {
		t.Errorf("blank rationale: got %d, want 400", rec.Code)
	}
	if et.Calls != 0 {
		t.Fatalf("a malformed request reached the backend %d time(s)", et.Calls)
	}
}

// The surface is fail-closed and structurally admin-only: nil backend ⇒ 503; a plain (unelevated) session ⇒
// 401; a cross-origin POST ⇒ 403.
func TestEngineToggleFailClosedAndAdminOnly(t *testing.T) {
	// nil backend.
	rt, c := adminSurfaceRig(t, Deps{}, true)
	if rec := adminPostJSON(rt, c, "/v1/policy/engine-toggle", `{"enable":false,"reason":"r"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil backend: got %d, want 503", rec.Code)
	}
	// plain session, not elevated.
	et := &MemEngineToggler{}
	rt2, c2 := adminSurfaceRig(t, Deps{EngineToggle: et}, false)
	if rec := adminPostJSON(rt2, c2, "/v1/policy/engine-toggle", `{"enable":false,"reason":"r"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("plain session on engine toggle: got %d, want 401", rec.Code)
	}
	// cross-origin.
	rt3, c3 := adminSurfaceRig(t, Deps{EngineToggle: &MemEngineToggler{}}, true)
	req := httptest.NewRequest(http.MethodPost, "/v1/policy/engine-toggle", strings.NewReader(`{"enable":false,"reason":"r"}`))
	req.AddCookie(c3)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	rt3.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin engine toggle: got %d, want 403", rec.Code)
	}
	if et.Calls != 0 {
		t.Fatalf("an unauthorized/unelevated request reached the backend")
	}
}
