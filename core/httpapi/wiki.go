package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// The wiki read surface (spec/006 REQ-521): the living knowledge base the console renders — three
// sections composed from what the system actually recorded. "lessons" are the distilled
// resolved-incident corpus entries the worker maintains (the SAME file the retrieval plane reloads —
// TG_KNOWLEDGE_FILE); "runbooks" are the curated operator pages embedded in the binary (docs/wiki);
// "skills" is the production skill library referenced from the existing SkillsReader (the console
// links through to #skills). Read-only (AuthReadOnly). Nil reader = 503 (the wiki is optional wiring;
// the console renders "wiki unavailable", never a fabricated knowledge base). An absent or empty
// corpus file is an HONEST empty lessons section — nothing is ever invented (INV-15).

// WikiLesson is one distilled resolved-incident corpus entry as the console lists it. The fields are
// exactly the knowledge.Incident schema — the corpus records no confidence score and no timestamp, so
// none is served (an invented one would violate INV-15).
type WikiLesson struct {
	Slug        string   `json:"slug"` // the external_ref — the lesson's citable identity
	ExternalRef string   `json:"external_ref"`
	Host        string   `json:"host,omitempty"`
	AlertRule   string   `json:"alert_rule,omitempty"`
	Site        string   `json:"site,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Resolution  string   `json:"resolution,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// WikiDoc is one embedded runbook page in the index (slug + title only; the body is the detail).
type WikiDoc struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
}

// WikiSkillRef is one production-library row the wiki links through to the skills surface.
type WikiSkillRef struct {
	Name              string `json:"name"`
	Kind              string `json:"kind"`
	Pinned            bool   `json:"pinned"`
	ProductionVersion string `json:"production_version,omitempty"`
	ActiveTrial       bool   `json:"active_trial"`
}

// WikiIndex is the GET /v1/wiki envelope. LessonTotal carries the true corpus size so a bounded
// lessons list never misrepresents how much the system has learned.
type WikiIndex struct {
	Lessons     []WikiLesson `json:"lessons"`
	LessonTotal int          `json:"lesson_total"`
	Runbooks    []WikiDoc    `json:"runbooks"`
	// Articles are the COMPILED pages — one per host TG has triaged, derived from the spine by the
	// worker's wikicompile lane. Unlike runbooks (authored, embedded at build time) and lessons
	// (distilled from confirmed-clean resolutions), these are regenerated from live data, so
	// ArticlesCompiledAt is what tells an operator how old the answer is.
	Articles     []WikiDoc `json:"articles"`
	ArticleTotal int       `json:"article_total"`
	// A POINTER, not a time.Time: `omitempty` does not apply to a struct, so a zero value serializes as
	// "0001-01-01T00:00:00Z" — a console showing "compiled 1 Jan 0001" over a wiki that has never been
	// compiled is exactly the invented-value pathology this surface exists to prevent. nil = never compiled.
	ArticlesCompiledAt *time.Time     `json:"articles_compiled_at,omitempty"`
	Skills             []WikiSkillRef `json:"skills"`
	SkillsAvailable    bool           `json:"skills_available"` // false = the skill store is not wired (503 on /v1/skills)
}

// WikiPage is one page (GET /v1/wiki/{slug}): a lesson detail or an embedded runbook, body as markdown.
type WikiPage struct {
	Slug  string            `json:"slug"`
	Title string            `json:"title"`
	Kind  string            `json:"kind"` // "lesson" | "runbook"
	Body  string            `json:"body"` // markdown
	Meta  map[string]string `json:"meta,omitempty"`
}

// WikiReader serves the lessons + runbooks sections (the skills section is joined in by the handler
// from the existing SkillsReader). Page returns found=false for an unknown slug (404, never an empty
// fabrication).
type WikiReader interface {
	WikiIndex(ctx context.Context) (WikiIndex, error)
	WikiPage(ctx context.Context, slug string) (WikiPage, bool, error)
}

// wikiHandler serves GET /v1/wiki — the index of everything the system knows, honestly sectioned.
func (d Deps) wikiHandler(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Wiki == nil {
		http.Error(w, "wiki unavailable", http.StatusServiceUnavailable)
		return
	}
	idx, err := d.Wiki.WikiIndex(r.Context())
	if err != nil {
		http.Error(w, "wiki index failed", http.StatusInternalServerError)
		return
	}
	// The skills section is a reference join over the existing skill read surface: present when the
	// store is wired, honestly flagged absent when it is not — never an invented library.
	if d.Skills != nil {
		if list, lerr := d.Skills.ListSkills(r.Context()); lerr == nil {
			idx.SkillsAvailable = true
			for _, s := range list {
				idx.Skills = append(idx.Skills, WikiSkillRef{
					Name: s.Name, Kind: s.Kind, Pinned: s.Pinned,
					ProductionVersion: s.ProductionVersion, ActiveTrial: s.ActiveTrial,
				})
				// STORE-BACKED RUNBOOKS join the runbooks section (TG-476, epic TG-114 C-7): every
				// runbook-class row with a PRODUCTION version is a wiki page under the namespaced slug
				// runbook/<name> (resolved by GET /v1/wiki/runbook/{name}), titled by the skill name.
				// Fail-closed by construction: a nil/erroring store reaches neither this loop nor the
				// join above, so the embedded pages serve exactly as before — and with NO runbook rows
				// this appends nothing, leaving the index byte-identical to the pre-store wiki (the
				// empty-input oracle in wiki_test.go pins both).
				if s.ArtifactClass == string(skillstore.ClassRunbook) && s.ProductionVersion != "" {
					idx.Runbooks = append(idx.Runbooks, WikiDoc{Slug: "runbook/" + s.Name, Title: s.Name})
				}
			}
		}
	}
	// Empty sections serialize as [], not null — an empty state the console can render honestly.
	if idx.Lessons == nil {
		idx.Lessons = []WikiLesson{}
	}
	if idx.Runbooks == nil {
		idx.Runbooks = []WikiDoc{}
	}
	if idx.Articles == nil {
		idx.Articles = []WikiDoc{}
	}
	if idx.Skills == nil {
		idx.Skills = []WikiSkillRef{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(idx)
}

// wikiPageHandler serves GET /v1/wiki/{slug} (the slug is resolved by exact lookup — an embedded
// runbook first, then a lesson by external_ref; never interpolated anywhere).
func (d Deps) wikiPageHandler(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Wiki == nil {
		http.Error(w, "wiki unavailable", http.StatusServiceUnavailable)
		return
	}
	// chi resolves {slug}; the path-suffix fallback keeps direct handler invocation (tests) working.
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		slug = strings.TrimPrefix(r.URL.Path, "/v1/wiki/")
	}
	if slug == "" || strings.Contains(slug, "/") {
		http.Error(w, "wiki slug required", http.StatusBadRequest)
		return
	}
	page, ok, err := d.Wiki.WikiPage(r.Context(), slug)
	if err != nil {
		http.Error(w, "wiki page failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "unknown wiki page", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(page)
}

// wikiRunbookPageHandler serves GET /v1/wiki/runbook/{name} — the STORE-BACKED runbook pages (TG-476):
// the production version of a runbook-class skill row, rendered as a wiki page under the namespaced slug
// the index lists (runbook/<name>). A separate route rather than a slug convention inside {slug} because
// chi matches path segments: the two-segment path can never collide with an embedded page, a lesson
// external_ref, or a compiled host article, so the store namespace can never shadow authored or derived
// pages (the ordering concern wiki_read.go documents does not even arise). Resolution is fail-closed and
// class-checked: no store ⇒ 503; unknown name, a NON-runbook class (a skill body is seed material, not a
// wiki page), or no production version ⇒ 404 — never a fabricated page.
func (d Deps) wikiRunbookPageHandler(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Skills == nil {
		http.Error(w, "skill store unavailable", http.StatusServiceUnavailable)
		return
	}
	name := chi.URLParam(r, "name")
	if name == "" {
		name = strings.TrimPrefix(r.URL.Path, "/v1/wiki/runbook/")
	}
	if name == "" || strings.Contains(name, "/") {
		http.Error(w, "runbook name required", http.StatusBadRequest)
		return
	}
	det, ok, err := d.Skills.SkillDetail(r.Context(), name)
	if err != nil {
		http.Error(w, "wiki page failed", http.StatusInternalServerError)
		return
	}
	if !ok || det.ArtifactClass != string(skillstore.ClassRunbook) {
		http.Error(w, "unknown wiki page", http.StatusNotFound)
		return
	}
	for _, v := range det.Versions {
		if v.Status != string(skillstore.StatusProduction) {
			continue
		}
		// Meta carries the page's REAL provenance — the version identity an operator can take back to
		// the skills surface — never invented fields.
		meta := map[string]string{"version": v.Version, "artifact_class": det.ArtifactClass}
		if v.Author != "" {
			meta["author"] = v.Author
		}
		if det.Description != "" {
			meta["description"] = det.Description
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(WikiPage{
			Slug: "runbook/" + name, Title: name, Kind: "runbook", Body: v.Body, Meta: meta,
		})
		return
	}
	// A runbook with drafts but no production version has no published page yet.
	http.Error(w, "unknown wiki page", http.StatusNotFound)
}
