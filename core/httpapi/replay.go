package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
)

// replayHandler handles a session-replay request. It is a read path plus a workflow-mint path: it
// loads an immutable read-only ContextSnapshot by external_ref under the caller's authority, and if
// found starts a NEW gated workflow that re-runs the full gate from zero. It NEVER resumes a mutating
// session and never accepts caller-supplied mutable state — there is no resume-with-prompt primitive
// (REQ-501, closes H-01/P0-2).
//
// An unknown id and an id the caller has no authority over are both mapped to 404 by SnapshotStore
// returning found=false, so an unauthorized id is indistinguishable from a missing one (REQ-504).
func (d Deps) replayHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	ref := chi.URLParam(r, "external_ref")
	snap, found, err := d.Snapshots.Get(r.Context(), ref, p)
	if err != nil {
		http.Error(w, "replay error", http.StatusInternalServerError)
		return
	}
	if !found {
		// Unknown OR unauthorized — reveal nothing that distinguishes the two (REQ-504).
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	workflowID, err := d.Starter.StartFromSnapshot(r.Context(), snap)
	if err != nil {
		http.Error(w, "replay error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"workflow_id":  workflowID,
		"external_ref": ref,
		"note":         "new gated workflow minted from a read-only snapshot; no mutating session resumed",
	})
}
