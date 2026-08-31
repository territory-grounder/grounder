package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/auth"
)

// runbookSkillsFixture is a library carrying every class shape the wiki merge must discriminate:
// a published runbook (production), a draft-only runbook (no page yet), and the non-runbook classes
// that must never become wiki pages.
func runbookSkillsFixture() fakeSkills {
	return fakeSkills{
		list: []SkillSummary{
			{Name: "triage-protocol", Kind: "behavioral", ArtifactClass: "skill", Position: 5, ProductionVersion: "1.1.0"},
			{Name: "disk-full-runbook", Kind: "catalog", ArtifactClass: "runbook", Position: 40, ProductionVersion: "1.0.0",
				Description: "How to drain and verify a full guest disk."},
			{Name: "draft-only-runbook", Kind: "catalog", ArtifactClass: "runbook", Position: 41},
			{Name: "judge-rubric", Kind: "catalog", ArtifactClass: "rubric", Pinned: true, Position: 1000, ProductionVersion: "3"},
		},
		detail: map[string]SkillDetailView{
			"disk-full-runbook": {
				SkillSummary: SkillSummary{Name: "disk-full-runbook", ArtifactClass: "runbook", ProductionVersion: "1.0.0",
					Description: "How to drain and verify a full guest disk."},
				Versions: []SkillVersionView{
					{ID: 9, Version: "1.0.0", Status: "production", Body: "## Guest disk full\n\n1. df -h …", Author: "operator:kyriakosp"},
					{ID: 8, Version: "0.9.0", Status: "retired", Body: "old"},
				},
			},
			"draft-only-runbook": {
				SkillSummary: SkillSummary{Name: "draft-only-runbook", ArtifactClass: "runbook"},
				Versions:     []SkillVersionView{{ID: 11, Version: "0.1.0", Status: "draft", Body: "wip"}},
			},
			"triage-protocol": {
				SkillSummary: SkillSummary{Name: "triage-protocol", ArtifactClass: "skill", ProductionVersion: "1.1.0"},
				Versions:     []SkillVersionView{{ID: 2, Version: "1.1.0", Status: "production", Body: "seed material"}},
			},
		},
	}
}

// errSkills fails every read — the store-down shape the wiki must serve THROUGH, not with.
type errSkills struct{}

func (errSkills) ListSkills(context.Context) ([]SkillSummary, error) {
	return nil, errors.New("store down")
}
func (errSkills) SkillDetail(context.Context, string) (SkillDetailView, bool, error) {
	return SkillDetailView{}, false, errors.New("store down")
}
func (errSkills) ListTrials(context.Context) ([]TrialView, error) {
	return nil, errors.New("store down")
}

// TG-476: every PRODUCTION runbook-class row is a wiki page under runbook/<name>, titled by the skill
// name, appended after the embedded set. A draft-only runbook has no page yet, and skill/rubric/prompt
// rows never enter the runbooks section at all.
func TestWikiIndexMergesStoreRunbooks(t *testing.T) {
	w := httptest.NewRecorder()
	Deps{Wiki: wikiFixture(), Skills: runbookSkillsFixture()}.wikiHandler(w, httptest.NewRequest("GET", "/v1/wiki", nil), auth.Principal{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var idx WikiIndex
	if err := json.Unmarshal(w.Body.Bytes(), &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Runbooks) != 2 {
		t.Fatalf("runbooks = embedded + the ONE published store runbook, got %+v", idx.Runbooks)
	}
	if idx.Runbooks[0].Slug != "triage-protocol" {
		t.Fatalf("embedded pages must stay first, got %+v", idx.Runbooks[0])
	}
	if idx.Runbooks[1].Slug != "runbook/disk-full-runbook" || idx.Runbooks[1].Title != "disk-full-runbook" {
		t.Fatalf("the published runbook must list as runbook/<name> titled by the skill name, got %+v", idx.Runbooks[1])
	}
	for _, rb := range idx.Runbooks {
		if strings.Contains(rb.Slug, "draft-only") || strings.Contains(rb.Slug, "judge-rubric") || rb.Slug == "runbook/triage-protocol" {
			t.Fatalf("only PRODUCTION RUNBOOK-class rows may become pages, got %+v", idx.Runbooks)
		}
	}
}

// FAIL-CLOSED (the empty-input mutation oracle): with the store DOWN the wiki serves BYTE-IDENTICALLY
// to the store-not-wired deployment — embedded pages intact, skills honestly absent — and with a store
// that has NO runbook rows the runbooks section is exactly the embedded set. The wiki must never lose
// its authored pages to a store fault, and with no runbook rows the merge must add nothing.
func TestWikiRunbookMergeFailsClosed(t *testing.T) {
	serve := func(d Deps) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		d.wikiHandler(w, httptest.NewRequest("GET", "/v1/wiki", nil), auth.Principal{})
		return w
	}
	down, unwired := serve(Deps{Wiki: wikiFixture(), Skills: errSkills{}}), serve(Deps{Wiki: wikiFixture()})
	if down.Code != http.StatusOK {
		t.Fatalf("a store fault must not fail the wiki, got %d", down.Code)
	}
	if !bytes.Equal(down.Body.Bytes(), unwired.Body.Bytes()) {
		t.Fatalf("store-down must serve byte-identically to store-unwired:\n%s\nvs\n%s", down.Body.String(), unwired.Body.String())
	}

	noRunbooks := serve(Deps{Wiki: wikiFixture(), Skills: skillsFixture()}) // the pre-TG-476 fixture: zero runbook-class rows
	var idx WikiIndex
	if err := json.Unmarshal(noRunbooks.Body.Bytes(), &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Runbooks) != 1 || idx.Runbooks[0].Slug != "triage-protocol" {
		t.Fatalf("with no runbook rows the runbooks section must be exactly the embedded set, got %+v", idx.Runbooks)
	}
}

// The runbook page route resolves ONLY a production runbook-class row: kind/body/meta from the real
// version, 404 for a non-runbook class (a skill body is seed material, not a wiki page), 404 with no
// production version, 503 with no store — and the store namespace never shadows the embedded pages
// (both routes dispatch through one real chi mux here, like production).
func TestWikiRunbookPage(t *testing.T) {
	d := Deps{Wiki: wikiFixture(), Skills: runbookSkillsFixture()}
	mux := newTestChiMux()
	pass := func(h func(Deps, http.ResponseWriter, *http.Request, auth.Principal)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { h(d, w, r, auth.Principal{}) }
	}
	mux.Handle("/v1/wiki/{slug}", pass(Deps.wikiPageHandler))
	mux.Handle("/v1/wiki/runbook/{name}", pass(Deps.wikiRunbookPageHandler))

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/wiki/runbook/disk-full-runbook", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("published runbook page: status = %d (%s)", w.Code, w.Body.String())
	}
	var p WikiPage
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Slug != "runbook/disk-full-runbook" || p.Title != "disk-full-runbook" || p.Kind != "runbook" {
		t.Fatalf("page identity wrong: %+v", p)
	}
	if !strings.Contains(p.Body, "df -h") || p.Meta["version"] != "1.0.0" || p.Meta["author"] != "operator:kyriakosp" {
		t.Fatalf("the page must carry the production body and its real provenance, got %+v", p)
	}

	for name, want := range map[string]int{
		"triage-protocol":    http.StatusNotFound, // skill-class: seed material, never a wiki page
		"draft-only-runbook": http.StatusNotFound, // no production version: no published page yet
		"ghost":              http.StatusNotFound,
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/wiki/runbook/"+name, nil))
		if w.Code != want {
			t.Fatalf("%s: status = %d, want %d", name, w.Code, want)
		}
	}

	// The embedded page still resolves on the one-segment route beside the store namespace.
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/wiki/triage-protocol", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"runbook"`) {
		t.Fatalf("embedded page must keep resolving, got %d", w.Code)
	}

	w = httptest.NewRecorder()
	Deps{Wiki: wikiFixture()}.wikiRunbookPageHandler(w, httptest.NewRequest("GET", "/v1/wiki/runbook/x", nil), auth.Principal{})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store: status = %d, want 503", w.Code)
	}
}
