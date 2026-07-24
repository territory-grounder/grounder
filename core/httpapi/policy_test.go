package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/auth"
)

func boolPtr(b bool) *bool      { return &b }
func f64Ptr(f float64) *float64 { return &f }

func policyFixture() *MemPolicyReader {
	return &MemPolicyReader{
		Decisions: []PolicyDecision{
			{RuleID: "svc-restart", Verdict: "auto", BandMode: "respect", ComposedBand: "AUTO", MinConfidence: 0.6, ActionID: "policy-decision:svc-restart:auto", Mode: "Full-auto", CreatedAt: "2026-07-19T10:10:00Z"},
			{RuleID: "rm-guard", Verdict: "deny", ComposedBand: "POLL_PAUSE", MinConfidence: 0.6, ActionID: "policy-decision:rm-guard:deny", Mode: "Semi-auto", CreatedAt: "2026-07-19T10:09:00Z"},
			{RuleID: "", Verdict: "approve", BandMode: "respect", ComposedBand: "POLL_PAUSE", MinConfidence: 0.6, ActionID: "policy-decision:no-rule:approve", Mode: "Shadow", CreatedAt: "2026-07-19T10:08:00Z"},
		},
		Mode: PolicyMode{Mode: "Semi-auto", Persisted: true, MayAutoActuate: true, RequiresHumanVote: false, Posture: "auto-actuation permitted for graduated op-classes"},
		Classes: []PolicyGraduationClass{
			{OpClass: "service-restart", Level: "auto", CleanRunCount: 0, LastOutcome: "verified_clean", UpdatedAt: "2026-07-19T10:00:00Z"},
			{OpClass: "config-reload", Level: "approve", CleanRunCount: 3, LastOutcome: "verified_clean", UpdatedAt: "2026-07-19T09:00:00Z"},
		},
		Rules: PolicyRulesPage{
			Present: true, RuleCount: 2, UpdatedBy: "admin:root", UpdatedAt: "2026-07-19T08:00:00Z",
			Rules: []PolicyRule{
				{ID: "svc-restart", Verdict: "auto", Match: PolicyRuleMatch{OpClass: "service-restart", Selector: "host_glob:nl*", Reversible: boolPtr(true)}, MinConfidence: f64Ptr(0.6), BandMode: "respect"},
				{ID: "rm-guard", Verdict: "deny", Match: PolicyRuleMatch{ArgvPattern: "rm -rf"}},
			},
		},
	}
}

// T-015-12: /v1/policy/decisions serves the recent decision audit newest-first; the ?limit= cap is applied by
// the handler and forwarded to the reader.
func TestPolicyDecisionsShapeAndCap(t *testing.T) {
	f := policyFixture()
	w := httptest.NewRecorder()
	Deps{Policy: f}.policyDecisionsHandler(w, httptest.NewRequest("GET", "/v1/policy/decisions", nil), auth.Principal{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var page PolicyDecisionsPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Decisions) != 3 || page.Decisions[0].Verdict != "auto" {
		t.Fatalf("decisions shape wrong: %+v", page.Decisions)
	}
	if f.LastLimit != policyPageDefault {
		t.Fatalf("default read must pass limit=%d, got %d", policyPageDefault, f.LastLimit)
	}
	// ?limit= over the cap is clamped to policyPageLimit BEFORE the read.
	f = policyFixture()
	w = httptest.NewRecorder()
	Deps{Policy: f}.policyDecisionsHandler(w, httptest.NewRequest("GET", "/v1/policy/decisions?limit=99999", nil), auth.Principal{})
	if f.LastLimit != policyPageLimit {
		t.Fatalf("limit must be capped at %d, got %d", policyPageLimit, f.LastLimit)
	}
}

// T-015-12: /v1/policy/mode serves the active mode + honest posture.
func TestPolicyModeShape(t *testing.T) {
	w := httptest.NewRecorder()
	Deps{Policy: policyFixture()}.policyModeHandler(w, httptest.NewRequest("GET", "/v1/policy/mode", nil), auth.Principal{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var m PolicyMode
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Mode != "Semi-auto" || !m.MayAutoActuate || m.RequiresHumanVote {
		t.Fatalf("mode posture wrong: %+v", m)
	}
}

// T-015-12: /v1/policy/graduation serves the per-op-class ladder; /v1/policy/rules serves rules-as-data.
func TestPolicyGraduationAndRulesShape(t *testing.T) {
	w := httptest.NewRecorder()
	Deps{Policy: policyFixture()}.policyGraduationHandler(w, httptest.NewRequest("GET", "/v1/policy/graduation", nil), auth.Principal{})
	var gp PolicyGraduationPage
	if err := json.Unmarshal(w.Body.Bytes(), &gp); err != nil {
		t.Fatal(err)
	}
	if len(gp.Classes) != 2 || gp.Classes[0].Level != "auto" {
		t.Fatalf("graduation shape wrong: %+v", gp.Classes)
	}
	w = httptest.NewRecorder()
	Deps{Policy: policyFixture()}.policyRulesHandler(w, httptest.NewRequest("GET", "/v1/policy/rules", nil), auth.Principal{})
	var rp PolicyRulesPage
	if err := json.Unmarshal(w.Body.Bytes(), &rp); err != nil {
		t.Fatal(err)
	}
	if !rp.Present || rp.RuleCount != 2 || len(rp.Rules) != 2 || rp.Rules[0].Match.Selector != "host_glob:nl*" {
		t.Fatalf("rules shape wrong: %+v", rp)
	}
}

// An empty spine returns an honest empty result on every collection route — never a fabricated row (INV-15).
func TestPolicyEmptyIsHonest(t *testing.T) {
	empty := &MemPolicyReader{}
	for _, tc := range []struct {
		path    string
		handler func(Deps, http.ResponseWriter, *http.Request, auth.Principal)
		wantKey string
	}{
		{"/v1/policy/decisions", Deps.policyDecisionsHandler, `"decisions":[]`},
		{"/v1/policy/graduation", Deps.policyGraduationHandler, `"classes":[]`},
		{"/v1/policy/rules", Deps.policyRulesHandler, `"rules":[]`},
	} {
		w := httptest.NewRecorder()
		tc.handler(Deps{Policy: empty}, w, httptest.NewRequest("GET", tc.path, nil), auth.Principal{})
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", tc.path, w.Code)
		}
		if !strings.Contains(w.Body.String(), tc.wantKey) {
			t.Fatalf("%s: empty must serialize %s, got %s", tc.path, tc.wantKey, w.Body.String())
		}
	}
}

// A nil reader is 503 on every policy route (optional wiring; never a fabricated projection).
func TestPolicyUnavailable(t *testing.T) {
	for _, h := range []func(Deps, http.ResponseWriter, *http.Request, auth.Principal){
		Deps.policyDecisionsHandler, Deps.policyModeHandler, Deps.policyGraduationHandler, Deps.policyRulesHandler,
	} {
		w := httptest.NewRecorder()
		h(Deps{}, w, httptest.NewRequest("GET", "/v1/policy/x", nil), auth.Principal{})
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("nil reader: status = %d, want 503", w.Code)
		}
	}
}

// A reader error fails closed to 503 on every route, never a partial/fabricated body.
func TestPolicyReaderErrorFailsClosed(t *testing.T) {
	bad := &MemPolicyReader{Err: errPolicyBoom}
	for _, tc := range []struct {
		path    string
		handler func(Deps, http.ResponseWriter, *http.Request, auth.Principal)
	}{
		{"/v1/policy/decisions", Deps.policyDecisionsHandler},
		{"/v1/policy/mode", Deps.policyModeHandler},
		{"/v1/policy/graduation", Deps.policyGraduationHandler},
		{"/v1/policy/rules", Deps.policyRulesHandler},
	} {
		w := httptest.NewRecorder()
		tc.handler(Deps{Policy: bad}, w, httptest.NewRequest("GET", tc.path, nil), auth.Principal{})
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: reader error status = %d, want 503", tc.path, w.Code)
		}
	}
}

var errPolicyBoom = &policyErr{}

type policyErr struct{}

func (*policyErr) Error() string { return "boom" }

// CRUCIAL secret-leak guard (INV-13): no policy response may contain a secret / credential / argv / host
// field. The DTOs carry only non-secret rules-as-data + decision metadata by construction; this asserts the
// serialized JSON exposes NONE of the forbidden field names.
func TestPolicyNoSecretFieldEverLeaks(t *testing.T) {
	forbidden := []string{
		"secret", "password", "passphrase", "private_key", "privatekey", "private",
		"token", "bearer", "material", "key_material", "credential", "argv", "host",
		"secret_ref", "secretref", "value", "path", "key_ref", "key_ref_scheme",
	}
	d := Deps{Policy: policyFixture()}
	for _, tc := range []struct {
		path    string
		handler func(Deps, http.ResponseWriter, *http.Request, auth.Principal)
	}{
		{"/v1/policy/decisions", Deps.policyDecisionsHandler},
		{"/v1/policy/mode", Deps.policyModeHandler},
		{"/v1/policy/graduation", Deps.policyGraduationHandler},
		{"/v1/policy/rules", Deps.policyRulesHandler},
	} {
		w := httptest.NewRecorder()
		tc.handler(d, w, httptest.NewRequest("GET", tc.path, nil), auth.Principal{})
		var v any
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		assertNoForbiddenPolicyKey(t, tc.path, v, forbidden)
	}
}

func assertNoForbiddenPolicyKey(t *testing.T, where string, v any, forbidden []string) {
	t.Helper()
	switch node := v.(type) {
	case map[string]any:
		for k, child := range node {
			lk := strings.ToLower(k)
			for _, bad := range forbidden {
				if lk == bad {
					t.Fatalf("%s: response JSON exposes forbidden field %q — a policy read must never carry a secret/argv/host", where, k)
				}
			}
			assertNoForbiddenPolicyKey(t, where, child, forbidden)
		}
	case []any:
		for _, child := range node {
			assertNoForbiddenPolicyKey(t, where, child, forbidden)
		}
	}
}

// The four routes register AuthReadOnly through httpapi.Register (a route with no auth cannot exist —
// auth.Router.Handle panics on auth=none, INV-01). This proves they are declared read-only GET routes.
func TestPolicyRoutesRegisteredReadOnlyGet(t *testing.T) {
	rt := auth.NewRouter(&auth.Verifier{})
	Register(rt, Deps{Policy: policyFixture()})
	want := map[string]bool{
		"/v1/policy/decisions":  false,
		"/v1/policy/mode":       false,
		"/v1/policy/graduation": false,
		"/v1/policy/rules":      false,
	}
	for _, dr := range rt.DeclaredRoutes() {
		if _, ok := want[dr.Pattern]; ok {
			if dr.Method != http.MethodGet {
				t.Fatalf("%s declared method %q, want GET", dr.Pattern, dr.Method)
			}
			want[dr.Pattern] = true
		}
	}
	for p, seen := range want {
		if !seen {
			t.Fatalf("route %s was not registered as a declared GET route", p)
		}
	}
}
