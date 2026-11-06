package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// SKILL.md EXPORT (TG-55's remaining interchange half, TG-476; ADR-0012): GET /v1/skills/{name}/export
// renders the PRODUCTION row of any prose artifact as a SKILL.md document — YAML frontmatter
// (name/version/class/description/applies_when) + the markdown body — the 1:1 row↔SKILL.md mapping
// ADR-0012 promised ("conformance is an export format, not a rewrite"). Read-only (AuthReadOnly), served
// as text/markdown.
//
// DELIBERATE ASYMMETRY — export only, no POST import. The write path into the store is the existing
// draft flow (POST /v1/skills/{name}/versions + the closed transition verbs): it carries the mandatory
// rationale, the per-class body cap, the pin/class-law refusals and the ledger record, and a parallel
// "import" route would either duplicate or bypass that gate. A SKILL.md file enters as a draft through
// that same flow with a client-side frontmatter parse (a later, purely client-side step); the export
// here is the interchange format the round trip needs.

// skillExportHandler serves GET /v1/skills/{name}/export.
func (d Deps) skillExportHandler(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
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
		name = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/skills/"), "/export")
	}
	if name == "" || strings.Contains(name, "/") {
		http.Error(w, "skill name required", http.StatusBadRequest)
		return
	}
	det, ok, err := d.Skills.SkillDetail(r.Context(), name)
	if err != nil {
		http.Error(w, "skill detail failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "unknown skill", http.StatusNotFound)
		return
	}
	for _, v := range det.Versions {
		if v.Status != string(skillstore.StatusProduction) {
			continue
		}
		doc, rerr := renderSkillMD(det, v)
		if rerr != nil {
			http.Error(w, "skill export failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(doc))
		return
	}
	// No production version ⇒ nothing canonical to export: a draft is a proposal, not the artifact.
	http.Error(w, "no production version to export", http.StatusNotFound)
}

// renderSkillMD renders one production row as SKILL.md. Scalars are emitted with %q — a JSON-style
// double-quoted string is valid YAML, so an operator-authored description (or a version string with odd
// characters) can never break the frontmatter or smuggle a document boundary. applies_when is emitted as
// its canonical JSON object (JSON is a YAML subset; it is also byte-identical to what the draft POST
// accepts, keeping the round trip trivial). The class is the closed enum and the absent class exports as
// its DefaultClass reading, so every exported document states what the store would enforce.
func renderSkillMD(det SkillDetailView, v SkillVersionView) (string, error) {
	aw := skillstore.AppliesWhen{}
	if len(v.AppliesWhen) > 0 {
		if err := json.Unmarshal(v.AppliesWhen, &aw); err != nil {
			return "", fmt.Errorf("applies_when unreadable: %w", err)
		}
	}
	awJSON, err := json.Marshal(aw)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %q\n", det.Name)
	fmt.Fprintf(&b, "version: %q\n", v.Version)
	fmt.Fprintf(&b, "class: %s\n", skillstore.DefaultClass(skillstore.ArtifactClass(det.ArtifactClass)))
	if det.Description != "" {
		fmt.Fprintf(&b, "description: %q\n", det.Description)
	}
	fmt.Fprintf(&b, "applies_when: %s\n", awJSON)
	b.WriteString("---\n\n")
	b.WriteString(v.Body)
	if !strings.HasSuffix(v.Body, "\n") {
		b.WriteString("\n")
	}
	return b.String(), nil
}
