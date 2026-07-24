package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/auth"
)

type fakeWiki struct {
	idx   WikiIndex
	pages map[string]WikiPage
}

func (f fakeWiki) WikiIndex(context.Context) (WikiIndex, error) { return f.idx, nil }
func (f fakeWiki) WikiPage(_ context.Context, slug string) (WikiPage, bool, error) {
	p, ok := f.pages[slug]
	return p, ok, nil
}

func wikiFixture() fakeWiki {
	return fakeWiki{
		idx: WikiIndex{
			Lessons: []WikiLesson{
				{Slug: "librenms-4821", ExternalRef: "librenms-4821", Host: "dc1k8s-w3",
					AlertRule: "kubelet flap", Resolution: "no action — self-recovered", Tags: []string{"k8s"}},
			},
			LessonTotal: 1,
			Runbooks: []WikiDoc{
				{Slug: "triage-protocol", Title: "Triage protocol — how an alert becomes a gated proposal"},
			},
		},
		pages: map[string]WikiPage{
			"triage-protocol": {Slug: "triage-protocol", Title: "Triage protocol", Kind: "runbook",
				Body: "# Triage protocol\n\nIntake, novelty gate, predict-then-verify."},
			"librenms-4821": {Slug: "librenms-4821", Title: "kubelet flap on dc1k8s-w3", Kind: "lesson",
				Body: "## Resolution\n\nno action — self-recovered",
				Meta: map[string]string{"host": "dc1k8s-w3", "alert_rule": "kubelet flap"}},
		},
	}
}

// REQ-521: the index serves the three sections — lessons from the corpus, embedded runbooks, and the
// skills section joined from the EXISTING SkillsReader with its availability honestly flagged.
func TestWikiIndexSectionsAndSkillsJoin(t *testing.T) {
	w := httptest.NewRecorder()
	Deps{Wiki: wikiFixture(), Skills: skillsFixture()}.wikiHandler(w, httptest.NewRequest("GET", "/v1/wiki", nil), auth.Principal{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var idx WikiIndex
	if err := json.Unmarshal(w.Body.Bytes(), &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Lessons) != 1 || idx.Lessons[0].Slug != "librenms-4821" || idx.LessonTotal != 1 {
		t.Fatalf("lessons section wrong: %+v", idx.Lessons)
	}
	if len(idx.Runbooks) != 1 || idx.Runbooks[0].Slug != "triage-protocol" {
		t.Fatalf("runbooks section wrong: %+v", idx.Runbooks)
	}
	if !idx.SkillsAvailable || len(idx.Skills) != 2 || !idx.Skills[0].Pinned {
		t.Fatalf("skills join must carry the library with pinned flags, got %+v", idx.Skills)
	}
}

// A nil SkillsReader is an HONEST absence: the index still serves lessons + runbooks, with
// skills_available=false and an empty (never null, never invented) skills list.
func TestWikiIndexWithoutSkillStore(t *testing.T) {
	w := httptest.NewRecorder()
	Deps{Wiki: wikiFixture()}.wikiHandler(w, httptest.NewRequest("GET", "/v1/wiki", nil), auth.Principal{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var idx WikiIndex
	if err := json.Unmarshal(w.Body.Bytes(), &idx); err != nil {
		t.Fatal(err)
	}
	if idx.SkillsAvailable || len(idx.Skills) != 0 {
		t.Fatalf("no skill store must mean skills_available=false + empty list, got %+v", idx)
	}
	if !strings.Contains(w.Body.String(), `"skills":[]`) {
		t.Fatalf("empty skills must serialize as [], got %s", w.Body.String())
	}
}

// The page detail serves the markdown body as JSON {slug,title,kind,body,meta} for both kinds.
func TestWikiPageDetail(t *testing.T) {
	for slug, wantKind := range map[string]string{"triage-protocol": "runbook", "librenms-4821": "lesson"} {
		w := httptest.NewRecorder()
		Deps{Wiki: wikiFixture()}.wikiPageHandler(w, httptest.NewRequest("GET", "/v1/wiki/"+slug, nil), auth.Principal{})
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", slug, w.Code)
		}
		var p WikiPage
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if p.Slug != slug || p.Kind != wantKind || p.Body == "" {
			t.Fatalf("%s: page = %+v", slug, p)
		}
	}
}

// An unknown slug is a 404, never an empty fabrication; a nested path is a 400.
func TestWikiPageUnknownAndBadPath(t *testing.T) {
	w := httptest.NewRecorder()
	Deps{Wiki: wikiFixture()}.wikiPageHandler(w, httptest.NewRequest("GET", "/v1/wiki/ghost", nil), auth.Principal{})
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown slug: status = %d, want 404", w.Code)
	}
	w = httptest.NewRecorder()
	Deps{Wiki: wikiFixture()}.wikiPageHandler(w, httptest.NewRequest("GET", "/v1/wiki/a/b", nil), auth.Principal{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("nested path: status = %d, want 400", w.Code)
	}
}

// A nil reader is 503 on both routes (the wiki is optional wiring; never a fabricated knowledge base).
func TestWikiUnavailable(t *testing.T) {
	for _, h := range []func(Deps, http.ResponseWriter, *http.Request, auth.Principal){
		Deps.wikiHandler, Deps.wikiPageHandler,
	} {
		w := httptest.NewRecorder()
		h(Deps{}, w, httptest.NewRequest("GET", "/v1/wiki/x", nil), auth.Principal{})
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("nil reader: status = %d, want 503", w.Code)
		}
	}
}

// The routed-dispatch proof through a real chi mux: /v1/wiki reaches the index handler and
// /v1/wiki/{slug} reaches the page handler (same pattern as TestSkillsRoutedDispatch).
func TestWikiRoutedDispatch(t *testing.T) {
	d := Deps{Wiki: wikiFixture()}
	mux := newTestChiMux()
	pass := func(h func(Deps, http.ResponseWriter, *http.Request, auth.Principal)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { h(d, w, r, auth.Principal{}) }
	}
	mux.Handle("/v1/wiki", pass(Deps.wikiHandler))
	mux.Handle("/v1/wiki/{slug}", pass(Deps.wikiPageHandler))

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/wiki", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"lessons"`) {
		t.Fatalf("/v1/wiki must reach the index handler, got %d %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/wiki/triage-protocol", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"runbook"`) {
		t.Fatalf("/v1/wiki/{slug} must reach the page handler, got %d", w.Code)
	}
}

// A URL-encoded slash decodes to "/" and is refused — the traversal shape stays unreachable even
// through encoding (regression guard for chi's URLParam decoding).
func TestWikiPageEncodedSlashRefused(t *testing.T) {
	w := httptest.NewRecorder()
	mux := newTestChiMux()
	d := Deps{Wiki: wikiFixture()}
	mux.Handle("/v1/wiki/{slug}", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		d.wikiPageHandler(rw, r, auth.Principal{})
	}))
	req := httptest.NewRequest("GET", "/v1/wiki/a%2Fb", nil)
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
		t.Fatalf("encoded-slash slug must be refused, got %d", w.Code)
	}
}
