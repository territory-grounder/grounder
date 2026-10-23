package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/auth"
)

// ADR-0012/TG-55: the export is the exact SKILL.md rendering of the production row — frontmatter
// (name/version/class/description/applies_when) + body, served as markdown. GOLDEN on purpose: this is
// an interchange format, so its bytes are the contract.
func TestSkillExportRendersSkillMD(t *testing.T) {
	fx := fakeSkills{
		detail: map[string]SkillDetailView{
			"disk-full-runbook": {
				SkillSummary: SkillSummary{Name: "disk-full-runbook", ArtifactClass: "runbook",
					Description: "How to drain and verify a full guest disk.", ProductionVersion: "1.0.0"},
				Versions: []SkillVersionView{
					{ID: 12, Version: "1.1.0-draft", Status: "draft", Body: "unpublished"},
					{ID: 9, Version: "1.0.0", Status: "production", Body: "## Guest disk full\n\n1. df -h …",
						AppliesWhen: json.RawMessage(`{"phases":["investigate"]}`)},
				},
			},
		},
	}
	w := httptest.NewRecorder()
	Deps{Skills: fx}.skillExportHandler(w, httptest.NewRequest("GET", "/v1/skills/disk-full-runbook/export", nil), auth.Principal{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("content type = %q, want text/markdown", ct)
	}
	want := `---
name: "disk-full-runbook"
version: "1.0.0"
class: runbook
description: "How to drain and verify a full guest disk."
applies_when: {"phases":["investigate"]}
---

## Guest disk full

1. df -h …
`
	if got := w.Body.String(); got != want {
		t.Fatalf("SKILL.md rendering drifted:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// The absent class exports as its DefaultClass reading (skill), an empty description is OMITTED (never
// a fabricated empty line), and the PRODUCTION row is the one exported even when drafts are newer.
func TestSkillExportDefaultsAndOmissions(t *testing.T) {
	fx := fakeSkills{
		detail: map[string]SkillDetailView{
			"triage-protocol": {
				SkillSummary: SkillSummary{Name: "triage-protocol", ProductionVersion: "1.1.0"}, // class + description absent
				Versions: []SkillVersionView{
					{ID: 3, Version: "2.0.0", Status: "draft", Body: "newer draft"},
					{ID: 2, Version: "1.1.0", Status: "production", Body: "the production body"},
				},
			},
		},
	}
	w := httptest.NewRecorder()
	Deps{Skills: fx}.skillExportHandler(w, httptest.NewRequest("GET", "/v1/skills/triage-protocol/export", nil), auth.Principal{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	got := w.Body.String()
	if !strings.Contains(got, "class: skill\n") {
		t.Fatalf("the absent class must export as skill (DefaultClass), got:\n%s", got)
	}
	if strings.Contains(got, "description:") {
		t.Fatalf("an empty description must be omitted, got:\n%s", got)
	}
	if !strings.Contains(got, "the production body") || strings.Contains(got, "newer draft") {
		t.Fatalf("exactly the production row must export, got:\n%s", got)
	}
	if !strings.Contains(got, `applies_when: {}`) {
		t.Fatalf("an always-applies row must state the empty predicate explicitly, got:\n%s", got)
	}
}

// No production version / unknown skill are 404 (a draft is a proposal, not the artifact); a nil store
// is the honest 503.
func TestSkillExportRefusals(t *testing.T) {
	fx := fakeSkills{
		detail: map[string]SkillDetailView{
			"draft-only": {
				SkillSummary: SkillSummary{Name: "draft-only", ArtifactClass: "runbook"},
				Versions:     []SkillVersionView{{ID: 1, Version: "0.1.0", Status: "draft", Body: "wip"}},
			},
		},
	}
	for path, want := range map[string]int{
		"/v1/skills/draft-only/export": http.StatusNotFound,
		"/v1/skills/ghost/export":      http.StatusNotFound,
	} {
		w := httptest.NewRecorder()
		Deps{Skills: fx}.skillExportHandler(w, httptest.NewRequest("GET", path, nil), auth.Principal{})
		if w.Code != want {
			t.Fatalf("%s: status = %d, want %d", path, w.Code, want)
		}
	}
	w := httptest.NewRecorder()
	Deps{}.skillExportHandler(w, httptest.NewRequest("GET", "/v1/skills/x/export", nil), auth.Principal{})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil store: status = %d, want 503", w.Code)
	}
}
