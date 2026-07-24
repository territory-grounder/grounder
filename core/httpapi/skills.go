package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
)

// The skill-store read surface (spec/014 REQ-1311/1313): the versioned skill library the console
// renders — every version with its rationale log, author, source, eval scores, and ledger references,
// plus the trial dashboard's raw state. Read-only (AuthReadOnly); the write path is a separate task on
// the operator-session principal. Nil reader = 503 (the store is optional wiring; the console renders
// "store unavailable", never a fabricated library).

// SkillVersionView is one version row as the console sees it.
type SkillVersionView struct {
	ID          int64           `json:"id"`
	Version     string          `json:"version"`
	Status      string          `json:"status"`
	Body        string          `json:"body"`
	AppliesWhen json.RawMessage `json:"applies_when"`
	ContentHash string          `json:"content_hash"`
	Author      string          `json:"author"`
	Source      string          `json:"source"`
	Rationale   string          `json:"rationale"` // the append-only transition log
	EvalOffline json.RawMessage `json:"eval_offline,omitempty"`
	EvalOnline  json.RawMessage `json:"eval_online,omitempty"`
	LedgerSeq   int64           `json:"ledger_seq,omitempty"`
	CreatedAt   string          `json:"created_at"`
	StatusAt    string          `json:"status_changed_at"`
}

// SkillSummary is one library-list row.
type SkillSummary struct {
	Name              string `json:"name"`
	Kind              string `json:"kind"`
	Pinned            bool   `json:"pinned"`
	Position          int    `json:"position"`
	ProductionVersion string `json:"production_version,omitempty"`
	ProductionSource  string `json:"production_source,omitempty"`
	VersionCount      int    `json:"version_count"`
	ActiveTrial       bool   `json:"active_trial"`
}

// SkillDetailView is the full history view for one skill.
type SkillDetailView struct {
	SkillSummary
	Versions []SkillVersionView `json:"versions"` // newest first
}

// TrialView is one trial row with its assignment counts (the statistical dims deepen with the trial
// engine; this surface serves the raw state honestly).
type TrialView struct {
	ID              int64          `json:"id"`
	SkillName       string         `json:"skill_name"`
	Dimension       string         `json:"dimension"`
	Status          string         `json:"status"`
	CandidateIDs    []int64        `json:"candidate_ids"`
	ControlVersion  int64          `json:"control_version_id,omitempty"`
	MinSamples      int            `json:"min_samples_per_arm"`
	MinLift         float64        `json:"min_lift"`
	PThreshold      float64        `json:"p_threshold"`
	EndsAt          string         `json:"ends_at"`
	Assignments     map[string]int `json:"assignments"` // variant idx (string) → count; "-1" = control
	WinnerVersionID int64          `json:"winner_version_id,omitempty"`
	Note            string         `json:"note"`
	CreatedAt       string         `json:"created_at"`
	FinalizedAt     string         `json:"finalized_at,omitempty"`
}

// SkillsPage / SkillTrialsPage are the list envelopes.
type SkillsPage struct {
	Skills []SkillSummary `json:"skills"`
}
type SkillTrialsPage struct {
	Trials []TrialView `json:"trials"`
}

// SkillsReader is the read-side store the handlers serve from (pgx-backed in production, a fake in
// tests). Detail returns found=false for an unknown skill (404, never an empty fabrication).
type SkillsReader interface {
	ListSkills(ctx context.Context) ([]SkillSummary, error)
	SkillDetail(ctx context.Context, name string) (SkillDetailView, bool, error)
	ListTrials(ctx context.Context) ([]TrialView, error)
}

func writeSkillsJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// skillsHandler serves GET /v1/skills.
func (d Deps) skillsHandler(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Skills == nil {
		http.Error(w, "skill store unavailable", http.StatusServiceUnavailable)
		return
	}
	list, err := d.Skills.ListSkills(r.Context())
	if err != nil {
		http.Error(w, "skill list failed", http.StatusInternalServerError)
		return
	}
	writeSkillsJSON(w, SkillsPage{Skills: list})
}

// skillDetailHandler serves GET /v1/skills/{name} (the name is the path remainder — resolved by exact
// lookup, never interpolated into a query beyond a $1 parameter).
func (d Deps) skillDetailHandler(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Skills == nil {
		http.Error(w, "skill store unavailable", http.StatusServiceUnavailable)
		return
	}
	// chi resolves {name}; the path-suffix fallback keeps direct handler invocation (tests) working.
	name := chi.URLParam(r, "name")
	if name == "" {
		name = strings.TrimPrefix(r.URL.Path, "/v1/skills/")
	}
	if name == "" || strings.Contains(name, "/") || name == "trials" {
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
	writeSkillsJSON(w, det)
}

// skillTrialsHandler serves GET /v1/skills/trials.
func (d Deps) skillTrialsHandler(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Skills == nil {
		http.Error(w, "skill store unavailable", http.StatusServiceUnavailable)
		return
	}
	trials, err := d.Skills.ListTrials(r.Context())
	if err != nil {
		http.Error(w, "trial list failed", http.StatusInternalServerError)
		return
	}
	writeSkillsJSON(w, SkillTrialsPage{Trials: trials})
}
