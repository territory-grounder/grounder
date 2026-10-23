package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
)

type fakeSkills struct {
	list   []SkillSummary
	detail map[string]SkillDetailView
	trials []TrialView
}

func (f fakeSkills) ListSkills(context.Context) ([]SkillSummary, error) { return f.list, nil }
func (f fakeSkills) SkillDetail(_ context.Context, name string) (SkillDetailView, bool, error) {
	d, ok := f.detail[name]
	return d, ok, nil
}
func (f fakeSkills) ListTrials(context.Context) ([]TrialView, error) { return f.trials, nil }

func skillsFixture() fakeSkills {
	return fakeSkills{
		list: []SkillSummary{
			{Name: "conservative-remediation", Kind: "catalog", Pinned: true, Position: 4, ProductionVersion: "1.0.0", ProductionSource: "compiled-import", VersionCount: 1},
			{Name: "triage-protocol", Kind: "behavioral", Position: 5, ProductionVersion: "1.1.0", ProductionSource: "compiled-import", VersionCount: 2, ActiveTrial: true},
		},
		detail: map[string]SkillDetailView{
			"triage-protocol": {
				SkillSummary: SkillSummary{Name: "triage-protocol", ProductionVersion: "1.1.0", VersionCount: 2},
				Versions: []SkillVersionView{
					{ID: 2, Version: "1.1.0", Status: "production", Body: "b2", Rationale: "[production] welch p=0.03 lift=0.3", LedgerSeq: 41, EvalOnline: json.RawMessage(`{"mean":4.1}`)},
					{ID: 1, Version: "1.0.0", Status: "retired", Body: "b1", Rationale: "[retired] superseded by v1.1.0", LedgerSeq: 40},
				},
			},
		},
	}
}

// REQ-1313: the library detail exposes the version history with rationale logs, eval scores, and
// ledger references — the console renders provenance, never bare bodies.
func TestSkillDetailExposesRationaleScoresLedger(t *testing.T) {
	w := httptest.NewRecorder()
	Deps{Skills: skillsFixture()}.skillDetailHandler(w, httptest.NewRequest("GET", "/v1/skills/triage-protocol", nil), auth.Principal{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var det SkillDetailView
	if err := json.Unmarshal(w.Body.Bytes(), &det); err != nil {
		t.Fatal(err)
	}
	if len(det.Versions) != 2 || det.Versions[0].Version != "1.1.0" {
		t.Fatalf("history newest-first expected, got %+v", det.Versions)
	}
	if !strings.Contains(det.Versions[0].Rationale, "welch") || det.Versions[0].LedgerSeq != 41 {
		t.Fatal("rationale log + ledger seq must be exposed")
	}
	if det.Versions[0].EvalOnline == nil {
		t.Fatal("eval scores must be exposed")
	}
}

// An unknown skill is a 404, never an empty fabrication; a nested path is a 400.
func TestSkillDetailUnknownAndBadPath(t *testing.T) {
	w := httptest.NewRecorder()
	Deps{Skills: skillsFixture()}.skillDetailHandler(w, httptest.NewRequest("GET", "/v1/skills/ghost", nil), auth.Principal{})
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown skill: status = %d, want 404", w.Code)
	}
	w = httptest.NewRecorder()
	Deps{Skills: skillsFixture()}.skillDetailHandler(w, httptest.NewRequest("GET", "/v1/skills/a/b", nil), auth.Principal{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("nested path: status = %d, want 400", w.Code)
	}
}

// A nil reader is 503 on all three routes (store optional; never a fabricated library), and the list
// carries the pinned flag + active-trial badge the console renders.
func TestSkillsListAndUnavailable(t *testing.T) {
	for _, h := range []func(Deps, http.ResponseWriter, *http.Request, auth.Principal){
		Deps.skillsHandler, Deps.skillDetailHandler, Deps.skillTrialsHandler,
	} {
		w := httptest.NewRecorder()
		h(Deps{}, w, httptest.NewRequest("GET", "/v1/skills/x", nil), auth.Principal{})
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("nil reader: status = %d, want 503", w.Code)
		}
	}
	w := httptest.NewRecorder()
	Deps{Skills: skillsFixture()}.skillsHandler(w, httptest.NewRequest("GET", "/v1/skills", nil), auth.Principal{})
	var page SkillsPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Skills) != 2 || !page.Skills[0].Pinned || !page.Skills[1].ActiveTrial {
		t.Fatalf("list must carry pinned + trial badges, got %+v", page.Skills)
	}
}

// chiRouterForTest mounts the three skill routes on a bare chi mux with a pass-through principal —
// this proves DISPATCH (specificity, wildcards); the auth refusal path is proven by the spec/014
// acceptance oracle against the full auth.Router.
func newTestChiMux() *chi.Mux { return chi.NewRouter() }

func chiRouterForTest(d Deps) *chi.Mux {
	mux := newTestChiMux()
	pass := func(h func(Deps, http.ResponseWriter, *http.Request, auth.Principal)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { h(d, w, r, auth.Principal{}) }
	}
	mux.Handle("/v1/skills", pass(Deps.skillsHandler))
	mux.Handle("/v1/skills/trials", pass(Deps.skillTrialsHandler))
	mux.Handle("/v1/skills/{name}", pass(Deps.skillDetailHandler))
	return mux
}

// The routed-dispatch proof: through the REAL chi mux, /v1/skills/trials reaches the trials handler
// (chi's literal-over-wildcard specificity), /v1/skills/{name} reaches detail, and a trailing slash is
// refused — the review's untested-precedence gap, closed empirically.
func TestSkillsRoutedDispatch(t *testing.T) {
	mux := chiRouterForTest(Deps{Skills: fakeSkills{
		list:   []SkillSummary{{Name: "triage-protocol"}},
		detail: map[string]SkillDetailView{"triage-protocol": {SkillSummary: SkillSummary{Name: "triage-protocol"}}},
		trials: []TrialView{{ID: 7, SkillName: "triage-protocol", Status: "active", Assignments: map[string]int{}}},
	}})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/skills/trials", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"trials"`) {
		t.Fatalf("/v1/skills/trials must reach the trials handler, got %d %q", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "unknown skill") {
		t.Fatal("trials must never resolve as a skill name")
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/skills/triage-protocol", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"versions"`) {
		t.Fatalf("detail route must reach the detail handler, got %d", w.Code)
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/skills", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"skills"`) {
		t.Fatalf("list route must serve, got %d", w.Code)
	}
}
