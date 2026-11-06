package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/territory-grounder/grounder/core/auth"
)

func regimeFixture() *MemRegimeReader {
	return &MemRegimeReader{
		Resolutions: []RegimeResolution{
			{Target: "librespeed01", Regime: "native-ssh", Lane: "native-ssh", RuleID: "", Outcome: "resolved", CreatedAt: "2026-07-20T10:10:00Z"},
			{Target: "awx-edge", Regime: "awx-job", Lane: "awx-job", RuleID: "awx-edge", Outcome: "resolved", CreatedAt: "2026-07-20T10:09:00Z"},
			{Target: "mystery01", Outcome: "refused", CreatedAt: "2026-07-20T10:08:00Z"},
		},
		Actuations: []RegimeActuation{
			{ActionID: "act-1", Lane: "awx-job", JobTemplateID: "42", OpClass: "restart-service", JobID: "9001", TokenRef: "store:awx.launch", CreatedAt: "2026-07-20T10:07:00Z"},
		},
		DeferredVerdicts: []RegimeDeferredVerdict{
			{ActionID: "act-1", JobID: "9001", Status: "successful", Verdict: "match", Graduation: "verified_clean", CreatedAt: "2026-07-20T10:06:00Z"},
		},
		Coverage: []RegimeLaneCoverage{
			{Lane: "awx-job", Resolutions: 1, Actuations: 1},
			{Lane: "native-ssh", Resolutions: 1, Actuations: 0},
		},
	}
}

// T-017-7: GET /v1/regime returns the aggregated read view (resolutions, actuations, deferred verdicts, lane
// coverage) and forwards the default ?limit= to the reader.
func TestRegimeShapeAndCap(t *testing.T) {
	f := regimeFixture()
	w := httptest.NewRecorder()
	Deps{Regime: f}.regimeHandler(w, httptest.NewRequest("GET", "/v1/regime", nil), auth.Principal{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var page RegimePage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Resolutions) != 3 || page.Resolutions[0].Lane != "native-ssh" {
		t.Fatalf("resolutions shape wrong: %+v", page.Resolutions)
	}
	if page.Resolutions[2].Outcome != "refused" || page.Resolutions[2].Lane != "" {
		t.Fatalf("a refusal must carry no lane (fail closed): %+v", page.Resolutions[2])
	}
	if len(page.Actuations) != 1 || page.Actuations[0].TokenRef != "store:awx.launch" {
		t.Fatalf("actuations shape wrong: %+v", page.Actuations)
	}
	if len(page.DeferredVerdicts) != 1 || page.DeferredVerdicts[0].Graduation != "verified_clean" {
		t.Fatalf("deferred verdicts shape wrong: %+v", page.DeferredVerdicts)
	}
	if len(page.LaneCoverage) != 2 {
		t.Fatalf("lane coverage shape wrong: %+v", page.LaneCoverage)
	}
	if f.LastLimit != regimePageDefault {
		t.Fatalf("default read must pass limit=%d, got %d", regimePageDefault, f.LastLimit)
	}
}

// The ?limit= over the cap is clamped to regimePageLimit BEFORE the read.
func TestRegimeLimitCap(t *testing.T) {
	f := regimeFixture()
	w := httptest.NewRecorder()
	Deps{Regime: f}.regimeHandler(w, httptest.NewRequest("GET", "/v1/regime?limit=99999", nil), auth.Principal{})
	if f.LastLimit != regimePageLimit {
		t.Fatalf("limit must be capped at %d, got %d", regimePageLimit, f.LastLimit)
	}
}

// A nil reader fails closed to 503 (unwired store never fabricates a row).
func TestRegimeNilReader503(t *testing.T) {
	w := httptest.NewRecorder()
	Deps{}.regimeHandler(w, httptest.NewRequest("GET", "/v1/regime", nil), auth.Principal{})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil reader must 503, got %d", w.Code)
	}
}

// A reader error fails closed to 503, never a partial/fabricated page.
func TestRegimeReaderError503(t *testing.T) {
	f := regimeFixture()
	f.Err = errors.New("boom")
	w := httptest.NewRecorder()
	Deps{Regime: f}.regimeHandler(w, httptest.NewRequest("GET", "/v1/regime", nil), auth.Principal{})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("reader error must 503, got %d", w.Code)
	}
}

// An empty spine returns an honest empty page (never null slices), so the console renders cleanly at Shadow.
func TestRegimeEmptyIsHonest(t *testing.T) {
	w := httptest.NewRecorder()
	Deps{Regime: &MemRegimeReader{}}.regimeHandler(w, httptest.NewRequest("GET", "/v1/regime", nil), auth.Principal{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var page RegimePage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Resolutions == nil || page.Actuations == nil || page.DeferredVerdicts == nil || page.LaneCoverage == nil {
		t.Fatalf("empty page must carry empty (non-null) slices: %+v", page)
	}
	if len(page.Resolutions) != 0 || len(page.Actuations) != 0 {
		t.Fatalf("empty spine must be empty, got %+v", page)
	}
}

// A non-GET method is rejected (the surface is read-only).
func TestRegimeMethodNotAllowed(t *testing.T) {
	w := httptest.NewRecorder()
	Deps{Regime: regimeFixture()}.regimeHandler(w, httptest.NewRequest("POST", "/v1/regime", nil), auth.Principal{})
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST must be 405, got %d", w.Code)
	}
}
