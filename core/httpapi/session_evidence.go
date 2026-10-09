package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/trace"
)

// SessionEvidenceDTO is the console projection of one stored observation — the ground truth behind a recorded
// reasoning step (TG-272).
//
// Truncated/FullBytes travel because a clipped body that does not SAY it is clipped is a lie told by an
// evidence surface, which is the one place it costs the most. The console renders "showing 64 KiB of 210 KiB"
// from these two fields.
type SessionEvidenceDTO struct {
	Ref       string `json:"ref"`
	Cycle     int    `json:"cycle"`
	ID        string `json:"id"`
	Tool      string `json:"tool,omitempty"`
	Payload   string `json:"payload"`
	Truncated bool   `json:"truncated,omitempty"`
	FullBytes int    `json:"full_bytes,omitempty"`
}

// sessionEvidenceHandler serves GET /v1/sessions/{external_ref}/evidence/{evidence_id}.
//
// AUTHORITY IS THE WALK'S. This is registered under AuthTraceRead — the same gate as the walk itself — because
// the payload is strictly a detail OF that walk: anyone who may read that the agent ran check-host-services on
// a host may read what it returned. Handing it a weaker gate would make the citation a way around the tracer's
// own authority.
//
// A missing row is 404 and NOT an error: every session recorded before migration 0053 has a walk with no
// evidence behind it. The console says "not recorded" for those, which is true, rather than rendering an empty
// body that reads as "the tool returned nothing".
func (d Deps) sessionEvidenceHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if d.SessionEvidenceRead == nil {
		http.Error(w, "session evidence unavailable", http.StatusServiceUnavailable)
		return
	}
	ref := chi.URLParam(r, "external_ref")
	id := chi.URLParam(r, "evidence_id")
	if ref == "" || id == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// THE SESSION IS AUTHORIZED FIRST, THEN THE DETAIL IS READ. The evidence store is keyed by external_ref and
	// knows nothing about principals, so reading it directly would serve any session's observations to anyone
	// who may read ANY session. Resolving the walk through the authority-bearing reader first makes an
	// unauthorized ref indistinguishable from an unknown one, exactly as the walk endpoint already does.
	if d.SessionDetailRead != nil {
		if _, err := d.SessionDetailRead.SessionDetail(r.Context(), p, ref); err != nil {
			if errors.Is(err, trace.ErrNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, "session evidence unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	e, err := d.SessionEvidenceRead.Evidence(r.Context(), ref, id)
	if err != nil {
		if errors.Is(err, trace.ErrEvidenceNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "session evidence unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(SessionEvidenceDTO{
		Ref: e.ExternalRef, Cycle: e.Cycle, ID: e.EvidenceID, Tool: e.Tool,
		Payload: e.Payload, Truncated: e.Truncated, FullBytes: e.FullBytes,
	})
}
