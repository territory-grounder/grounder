package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
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
	Lessons         []WikiLesson   `json:"lessons"`
	LessonTotal     int            `json:"lesson_total"`
	Runbooks        []WikiDoc      `json:"runbooks"`
	Skills          []WikiSkillRef `json:"skills"`
	SkillsAvailable bool           `json:"skills_available"` // false = the skill store is not wired (503 on /v1/skills)
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
