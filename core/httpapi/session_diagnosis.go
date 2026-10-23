package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/trace"
)

// The TYPED CLAIM read surface (TG-201): one session's diagnosis — root cause, mechanism, the evidence that
// SUPPORTS it, the evidence that CONTRADICTS it, and the alternatives ruled out.
//
// WHY THIS ENDPOINT EXISTS. core/proposal/diagnosis.go gave the agent a field to say "observation lnms-x
// argues AGAINST my own root cause", and agent/loop.go binds every ref against the ToolResults the
// orchestrator actually gathered. None of it was reachable: no route projected it, so the operator read the
// same free-text rationale as before and the contradiction stayed in an unread transcript. That is the
// recorded A2 failure with an extra step, not a fix for it — the value of a typed claim is entirely in a
// HUMAN being able to check it.
//
// Observe-only: it runs after core/auth authenticated the caller, reaches no actuator, and returns a value no
// gate consumes (REQ-2002).

// SessionDiagnosisDTO is the console projection of one recorded claim.
//
// Contradicted and Uncited are SERVED, not left to the client. Both are answers to "is this citation
// grounded?", which is decided against the orchestrator's captured observations — one definition, on the
// server, so a second renderer cannot quietly adopt a laxer one (e.g. counting a non-empty id as a citation,
// which is precisely the fabricated-citation failure INV-11 exists for).
type SessionDiagnosisDTO struct {
	Ref           string                  `json:"ref"`
	RootCause     string                  `json:"root_cause"`
	Mechanism     string                  `json:"mechanism,omitempty"`
	Supporting    []DiagnosisRefDTO       `json:"supporting"`
	Contradicting []DiagnosisRefDTO       `json:"contradicting"`
	RuledOut      []DiagnosisAlternateDTO `json:"ruled_out"`
	// Contradicted is true iff the model cited GROUNDED evidence against its own root cause — the one fact
	// this whole type exists to make visible.
	Contradicted bool `json:"contradicted"`
	// Uncited counts assertions carrying no grounded observation: the "assertion 2 of 4 is uncited" the flat
	// evidence_ids list could never express.
	Uncited int `json:"uncited"`
	// Clipped says a stored text field was longer than the bound and was cut. Rendered, not bookkeeping.
	Clipped bool `json:"clipped,omitempty"`
}

// DiagnosisRefDTO is one assertion and the observation offered for it. Cited travels as its own field
// BECAUSE it is not derivable from ID: a model can name an id nobody captured, and that ref must render as an
// uncited assertion rather than as a citation.
type DiagnosisRefDTO struct {
	ID    string `json:"id,omitempty"`
	Claim string `json:"claim"`
	Cited bool   `json:"cited"`
}

// DiagnosisAlternateDTO is one alternative cause the model discarded and why.
type DiagnosisAlternateDTO struct {
	Cause  string `json:"cause"`
	Reason string `json:"reason,omitempty"`
	ID     string `json:"id,omitempty"`
	Cited  bool   `json:"cited"`
}

// ProjectSessionDiagnosis is the exported pure projection this endpoint serves — a stored claim mapped to the
// console DTO. Exposed so an oracle can assert the served shape without re-implementing request
// authentication. Pure: no I/O, no fabrication.
func ProjectSessionDiagnosis(d trace.SessionDiagnosis) SessionDiagnosisDTO {
	return sessionDiagnosisDTO(d)
}

func sessionDiagnosisDTO(d trace.SessionDiagnosis) SessionDiagnosisDTO {
	out := SessionDiagnosisDTO{
		Ref: d.ExternalRef, RootCause: d.RootCause, Mechanism: d.Mechanism,
		// Non-nil slices: an absent lane must serialize as [] and not null, because a renderer that has to
		// null-guard three lanes will eventually forget one, and the lane it forgets renders as nothing at all.
		Supporting:    make([]DiagnosisRefDTO, 0, len(d.Supporting)),
		Contradicting: make([]DiagnosisRefDTO, 0, len(d.Contradicting)),
		RuledOut:      make([]DiagnosisAlternateDTO, 0, len(d.RuledOut)),
		Contradicted:  d.HasGroundedContradiction(),
		Uncited:       d.UncitedAssertions(),
		Clipped:       d.Clipped,
	}
	for _, r := range d.Supporting {
		out.Supporting = append(out.Supporting, DiagnosisRefDTO{ID: r.ID, Claim: r.Claim, Cited: r.Cited})
	}
	// EVERY contradicting ref is projected, cited or not. Filtering the uncited ones out would be a defensible
	// -sounding "show only grounded evidence" rule that deletes the sentence "the model asserted something
	// against its own conclusion and could not ground it" — which an operator needs to read MORE, not less.
	for _, r := range d.Contradicting {
		out.Contradicting = append(out.Contradicting, DiagnosisRefDTO{ID: r.ID, Claim: r.Claim, Cited: r.Cited})
	}
	for _, a := range d.RuledOut {
		out.RuledOut = append(out.RuledOut, DiagnosisAlternateDTO{Cause: a.Cause, Reason: a.Reason, ID: a.ID, Cited: a.Cited})
	}
	return out
}

// sessionDiagnosisHandler serves GET /v1/sessions/{external_ref}/diagnosis.
//
// AUTHORITY IS THE WALK'S. It is registered under AuthTraceRead — the SAME elevated gate as the walk and the
// evidence citation — because the claim is a detail OF that walk: anyone who may read that the agent proposed
// a restart may read what it says the cause was. A weaker gate here would make the claim a way around the
// tracer's own authority, and this body quotes screened host output.
//
// A session with no recorded claim is 404 and NOT an error: a stand-down records no proposal, a model may
// return a proposal without a diagnosis, and every session older than migration 0056 has none. The console
// says "no typed claim was recorded" for those, which is true, instead of rendering an empty claim that reads
// as "the agent asserted nothing".
func (d Deps) sessionDiagnosisHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if d.SessionDiagnosisRead == nil {
		http.Error(w, "session diagnosis unavailable", http.StatusServiceUnavailable)
		return
	}
	ref := chi.URLParam(r, "external_ref")
	if ref == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// THE SESSION IS AUTHORIZED FIRST, THEN THE CLAIM IS READ — the same order the evidence route uses. The
	// diagnosis store is keyed by external_ref and knows nothing about principals, so reading it directly would
	// serve any session's claim to anyone who may read ANY session. Resolving the walk through the
	// authority-bearing reader first makes an unauthorized ref indistinguishable from an unknown one.
	if d.SessionDetailRead != nil {
		if _, err := d.SessionDetailRead.SessionDetail(r.Context(), p, ref); err != nil {
			if errors.Is(err, trace.ErrNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, "session diagnosis unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	rec, err := d.SessionDiagnosisRead.Diagnosis(r.Context(), ref)
	if err != nil {
		if errors.Is(err, trace.ErrDiagnosisNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "session diagnosis unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sessionDiagnosisDTO(rec))
}
