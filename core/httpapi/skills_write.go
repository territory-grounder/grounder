package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// The skill-store write surface (spec/014 REQ-1301/1311): create-draft + the three operator transitions
// (admit-to-trial, promote, retire), session-only — registered inside the browser-session block like
// /v1/vote, so a machine principal has NO write route at all. Rationale is mandatory at the surface
// (fast 400) and again in the state machine (the authority). Transitions execute in the WORKER (the
// ledger's single writer) through the skillwrite workflow; drafts are plain row inserts.

// SkillDraftRequest is the create-draft body.
type SkillDraftRequest struct {
	Version     string                 `json:"version"`
	Body        string                 `json:"body"`
	AppliesWhen skillstore.AppliesWhen `json:"applies_when"`
	Rationale   string                 `json:"rationale"`
}

// SkillTransitionOutcome is the synchronous transition result for the console.
type SkillTransitionOutcome struct {
	VersionID int64  `json:"version_id"`
	SkillName string `json:"skill_name"`
	Version   string `json:"version"`
	Status    string `json:"status"`
	LedgerSeq int64  `json:"ledger_seq"`
}

// SkillsWriter executes writes: drafts directly (a row insert), transitions via the worker.
type SkillsWriter interface {
	CreateDraft(ctx context.Context, skillName string, req SkillDraftRequest, operator string) (SkillVersionView, error)
	Transition(ctx context.Context, versionID int64, to skillstore.Status, rationale, operator string) (SkillTransitionOutcome, error)
}

// transitionForRoute maps the route verb to the target status — a closed table, never client text.
var transitionForRoute = map[string]skillstore.Status{
	"trial":   skillstore.StatusTrial,
	"promote": skillstore.StatusProduction,
	"retire":  skillstore.StatusRetired,
}

func (d Deps) skillWriteGuard(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if d.SkillsWrite == nil {
		http.Error(w, "skill write path unavailable", http.StatusServiceUnavailable)
		return false
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin write rejected", http.StatusForbidden)
		return false
	}
	return true
}

func skillWriteErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, skillstore.ErrRationaleRequired),
		errors.Is(err, skillstore.ErrBadPredicate),
		errors.Is(err, skillstore.ErrBodyBounds):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, skillstore.ErrPinnedSkill):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, skillstore.ErrBadTransition), errors.Is(err, skillstore.ErrProductionExists):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, skillstore.ErrNotFound):
		http.Error(w, "unknown version", http.StatusNotFound)
	default:
		http.Error(w, "skill write failed — retry", http.StatusServiceUnavailable)
	}
}

// skillDraftHandler serves POST /v1/skills/{name}/versions.
func (d Deps) skillDraftHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !d.skillWriteGuard(w, r) {
		return
	}
	name := chi.URLParam(r, "name")
	if name == "" {
		http.Error(w, "skill name required", http.StatusBadRequest)
		return
	}
	var req SkillDraftRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req); err != nil {
		http.Error(w, "malformed draft", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Rationale) == "" {
		http.Error(w, "rationale required — every version states why it exists", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Version) == "" || len(req.Version) > 64 {
		http.Error(w, "version required", http.StatusBadRequest)
		return
	}
	v, err := d.SkillsWrite.CreateDraft(r.Context(), name, req, operatorOf(p))
	if err != nil {
		skillWriteErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(v)
}

// skillTransitionHandler serves POST /v1/skills/versions/{id}/{verb} for the closed verb table.
func (d Deps) skillTransitionHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !d.skillWriteGuard(w, r) {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "version id required", http.StatusBadRequest)
		return
	}
	to, ok := transitionForRoute[chi.URLParam(r, "verb")]
	if !ok {
		http.Error(w, "unknown transition", http.StatusNotFound)
		return
	}
	var req struct {
		Rationale string `json:"rationale"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Rationale) == "" {
		http.Error(w, "rationale required — every transition states why", http.StatusBadRequest)
		return
	}
	out, err := d.SkillsWrite.Transition(r.Context(), id, to, strings.TrimSpace(req.Rationale), operatorOf(p))
	if err != nil {
		skillWriteErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// operatorOf derives the acting identity from the AUTHENTICATED principal — never from the body.
func operatorOf(p auth.Principal) string {
	return strings.TrimPrefix(p.SourceID, "operator:")
}

// SkillDraftHandlerForAcceptance / SkillTransitionHandlerForAcceptance expose the unexported handlers
// to the spec/014 acceptance oracle (an external package driving the REAL router/handlers). They add no
// behavior — the oracle must exercise exactly what production serves.
func (d Deps) SkillDraftHandlerForAcceptance() func(http.ResponseWriter, *http.Request, auth.Principal) {
	return d.skillDraftHandler
}

// SkillTransitionHandlerForAcceptance — see SkillDraftHandlerForAcceptance.
func (d Deps) SkillTransitionHandlerForAcceptance(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	d.skillTransitionHandler(w, r, p)
}
