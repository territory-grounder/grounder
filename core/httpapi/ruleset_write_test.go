package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A well-formed rules-as-data document (op-class match, closed-enum verdict) — parses through ParseRuleSet.
const validRuleset = `{"rules":[{"id":"r1","match":{"op_class":"restart-service"},"verdict":"approve"}]}`

// The admin write path: an admin-session operator posts a new ruleset; the handler validates it, derives the
// operator + admin proof from the authenticated principal (never the body), and forwards the order to the
// worker backend.
func TestRulesetWriteHappyPathUsesPrincipalIdentity(t *testing.T) {
	rw := &MemRulesetWriter{Outcome: RulesetWriteOutcome{Version: "bv-abc123", RuleCount: 1, UpdatedBy: "kyriakos", LedgerSeq: 77}}
	rt, c := adminSurfaceRig(t, Deps{RulesetWrite: rw}, true)

	body := `{"document":` + validRuleset + `,"expected_version":"bv-prev","rationale":"tighten restart policy"}`
	rec := adminPostJSON(rt, c, "/v1/policy/ruleset", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("ruleset write: got %d (%s)", rec.Code, rec.Body.String())
	}
	var out RulesetWriteOutcome
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("outcome decode: %v", err)
	}
	if out.Version != "bv-abc123" || out.RuleCount != 1 || out.LedgerSeq != 77 {
		t.Fatalf("outcome = %+v", out)
	}
	// The operator + admin proof MUST come from the server-authenticated principal, not the body.
	if rw.LastOperator != "kyriakos" {
		t.Fatalf("operator must be the authenticated session principal, got %q", rw.LastOperator)
	}
	if !rw.LastAdmin {
		t.Fatalf("admin proof must be forwarded (AuthAdminSession principal), got %v", rw.LastAdmin)
	}
	if rw.LastExpectedVersion != "bv-prev" || rw.LastRationale != "tighten restart policy" {
		t.Fatalf("forwarded order = expected_version=%q rationale=%q", rw.LastExpectedVersion, rw.LastRationale)
	}
	if strings.TrimSpace(string(rw.LastDocument)) != validRuleset {
		t.Fatalf("forwarded document = %q, want the object verbatim", rw.LastDocument)
	}
}

// The document may arrive as a JSON-encoded STRING as well as an object; both normalize to the same bytes.
func TestRulesetWriteAcceptsJSONStringDocument(t *testing.T) {
	rw := &MemRulesetWriter{Outcome: RulesetWriteOutcome{Version: "bv-str"}}
	rt, c := adminSurfaceRig(t, Deps{RulesetWrite: rw}, true)

	quoted, _ := json.Marshal(validRuleset) // the ruleset JSON, encoded as a JSON string value
	body := `{"document":` + string(quoted) + `}`
	rec := adminPostJSON(rt, c, "/v1/policy/ruleset", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("string-form document: got %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(string(rw.LastDocument)) != validRuleset {
		t.Fatalf("string-form document did not unquote to the inner ruleset JSON: %q", rw.LastDocument)
	}
}

// Surface validation: a malformed ruleset and an empty document fail closed with 400 BEFORE the backend is
// called — a bad ruleset governs actuation and must never reach the persist path.
func TestRulesetWriteRejectsMalformedBeforeBackend(t *testing.T) {
	rw := &MemRulesetWriter{Outcome: RulesetWriteOutcome{Version: "unused"}}
	rt, c := adminSurfaceRig(t, Deps{RulesetWrite: rw}, true)

	// unknown verdict — the killing surface check.
	bad := `{"document":{"rules":[{"id":"r1","match":{"op_class":"x"},"verdict":"nuke"}]}}`
	if rec := adminPostJSON(rt, c, "/v1/policy/ruleset", bad); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed ruleset: got %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	// unknown field — ParseRuleSet disallows unknown fields.
	if rec := adminPostJSON(rt, c, "/v1/policy/ruleset", `{"document":{"rules":[],"bogus":true}}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown-field ruleset: got %d, want 400", rec.Code)
	}
	// missing document.
	if rec := adminPostJSON(rt, c, "/v1/policy/ruleset", `{"rationale":"no doc"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing document: got %d, want 400", rec.Code)
	}
	if rw.Calls != 0 {
		t.Fatalf("a malformed/empty ruleset reached the backend %d time(s)", rw.Calls)
	}
}

// A stale compare-and-swap from the worker maps to 409; any other backend error to 503.
func TestRulesetWriteRefusalsMapToHonestStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"version-conflict", ErrRulesetVersionConflict, http.StatusConflict},
		{"generic-failure", errStub("worker down"), http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rw := &MemRulesetWriter{Err: tc.err}
			rt, c := adminSurfaceRig(t, Deps{RulesetWrite: rw}, true)
			rec := adminPostJSON(rt, c, "/v1/policy/ruleset", `{"document":`+validRuleset+`}`)
			if rec.Code != tc.want {
				t.Fatalf("%s: got %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// The surface is fail-closed and structurally admin-only: nil backend ⇒ 503; a plain (unelevated) session
// ⇒ 401; a cross-origin POST ⇒ 403; and none of these reach the backend.
func TestRulesetWriteFailClosedAndAdminOnly(t *testing.T) {
	// nil backend.
	rt, c := adminSurfaceRig(t, Deps{}, true)
	if rec := adminPostJSON(rt, c, "/v1/policy/ruleset", `{"document":`+validRuleset+`}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil backend: got %d, want 503", rec.Code)
	}
	// plain session, not elevated.
	rw := &MemRulesetWriter{}
	rt2, c2 := adminSurfaceRig(t, Deps{RulesetWrite: rw}, false)
	if rec := adminPostJSON(rt2, c2, "/v1/policy/ruleset", `{"document":`+validRuleset+`}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("plain session on ruleset write: got %d, want 401", rec.Code)
	}
	// cross-origin.
	rt3, c3 := adminSurfaceRig(t, Deps{RulesetWrite: &MemRulesetWriter{}}, true)
	req := httptest.NewRequest(http.MethodPost, "/v1/policy/ruleset", strings.NewReader(`{"document":`+validRuleset+`}`))
	req.AddCookie(c3)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	rt3.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin ruleset write: got %d, want 403", rec.Code)
	}
	if rw.Calls != 0 {
		t.Fatalf("an unauthorized/unelevated request reached the backend")
	}
}
