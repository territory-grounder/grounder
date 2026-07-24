package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/trace"
)

// The per-session detail read surface (spec/020 REQ-2011): the decision tracer's face. It assembles ONE
// incident's governed walk — classify → propose → predict → verify — from the durable correlation spine and
// serves it in decision-boundary order over an authenticated read. It is OBSERVE-ONLY: it runs only after
// core/auth authenticated the caller, reaches no actuator, and returns a value no gate consumes (REQ-2002).
// Every field it projects is drawn from a committed spine row (INV-15 — never a fabricated verdict) and
// carries no secret value, only references and schemes (INV-13). A nil reader (no durable spine wired) fails
// closed to 503; an unknown external_ref maps to 404, never an empty 200.

// SessionDetailReader assembles one session's trace under the principal's authority. Authority is resolved
// inside the implementation against the principal (INV-12); trace.ErrNotFound signals an unknown session.
type SessionDetailReader interface {
	SessionDetail(ctx context.Context, p auth.Principal, externalRef string) (trace.SessionTrace, error)
}

// SessionDetailDTO is the console-facing projection of a SessionTrace, keyed to match the Workflows view's
// WF_RUNS render contract so the live wiring (T-020-12) is a drop-in for the fixture. Fields the durable
// spine does not yet carry (finer per-gate/per-cycle granularity from T-020-7/8/9) are simply omitted rather
// than fabricated — the console renders what is present.
type SessionDetailDTO struct {
	ID      string           `json:"id"`    // external_ref (the run identity)
	Ref     string           `json:"ref"`   // external_ref
	Title   string           `json:"title"` // alert rule / incident title
	Host    string           `json:"host,omitempty"`
	Band    string           `json:"band,omitempty"`
	Status  string           `json:"status"` // classified | proposed | executed | stopped
	Risk    string           `json:"risk,omitempty"`
	Conf    float64          `json:"conf"`             // stated proposal confidence
	Action  string           `json:"action,omitempty"` // content-hashed action id
	Verdict string           `json:"verdict,omitempty"`
	Nodes   []SessionNodeDTO `json:"nodes"`
}

// SessionNodeDTO is one trace step keyed to the WF_RUNS node shape (t/lb/ts/st/src/pay/plan/gate/band/hash/
// verdict). It carries decision-grade fields only; credential fields, when present, are references/schemes
// (INV-13).
type SessionNodeDTO struct {
	T       string   `json:"t"`              // step kind (classify|propose|predict|verify)
	Lb      string   `json:"lb"`             // label
	Ts      string   `json:"ts,omitempty"`   // RFC3339 boundary timestamp
	St      string   `json:"st"`             // rendered step state (ok|pending|deviation)
	Src     string   `json:"src"`            // source boundary (== kind)
	Pay     string   `json:"pay,omitempty"`  // rationale / conclusion
	Plan    []string `json:"plan,omitempty"` // matched rules / skills provenance
	Gate    string   `json:"gate,omitempty"`
	Band    string   `json:"band,omitempty"`
	Hash    string   `json:"hash,omitempty"`
	Verdict string   `json:"verdict,omitempty"`
	Conf    float64  `json:"conf"`
	MinConf float64  `json:"min_conf"`
	// PlanOps is the sealed action's structured end-state diff on the commit boundary — add|change|destroy
	// ops projected from the manifest (INV-07), distinct from the flat Plan lane. References only (INV-13).
	PlanOps []PlanOpDTO `json:"plan_ops,omitempty"`
}

// PlanOpDTO is one structured end-state operation the console's commit boundary renders (op glyph + target).
type PlanOpDTO struct {
	Op string `json:"op"` // add | change | destroy
	T  string `json:"t"`  // human-readable target
}

// sessionDetailHandler serves GET /v1/sessions/{external_ref}. It runs only after core/auth authenticated the
// caller; a nil reader fails closed to 503, an unknown/unauthorized ref maps to 404 (indistinguishable), and
// any other read error is 503 rather than a fabricated body.
func (d Deps) sessionDetailHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if d.SessionDetailRead == nil {
		http.Error(w, "session detail unavailable", http.StatusServiceUnavailable)
		return
	}
	ref := chi.URLParam(r, "external_ref")
	if ref == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	tr, err := d.SessionDetailRead.SessionDetail(r.Context(), p, ref)
	if err != nil {
		if errors.Is(err, trace.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "session detail unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sessionDetailDTO(tr))
}

// ProjectSessionDetail is the exported pure projection the endpoint serves — a SessionTrace mapped to the
// console DTO. It is exposed so cross-package acceptance oracles can assert the ordered, non-secret
// projection the served body carries without re-implementing request authentication (the auth requirement
// itself is asserted separately over the real router). Pure: no I/O, no fabrication.
func ProjectSessionDetail(t trace.SessionTrace) SessionDetailDTO { return sessionDetailDTO(t) }

// sessionDetailDTO projects a SessionTrace to the console render contract. Pure mapping — no I/O, no
// fabrication: every field is copied from a committed spine value.
func sessionDetailDTO(t trace.SessionTrace) SessionDetailDTO {
	title := t.AlertRule
	if title == "" {
		title = t.ExternalRef
	}
	dto := SessionDetailDTO{
		ID: t.ExternalRef, Ref: t.ExternalRef, Title: title, Host: t.Host, Band: t.Band,
		Status: string(t.Status), Risk: t.RiskLevel, Conf: t.Confidence, Action: t.ActionID,
		Verdict: t.Verdict, Nodes: make([]SessionNodeDTO, 0, len(t.Steps)),
	}
	for _, s := range t.Steps {
		n := SessionNodeDTO{
			T: string(s.Kind), Lb: s.Label, St: stepState(s), Src: string(s.Kind),
			Pay: s.Reason, Gate: s.Gate, Band: s.Band, Verdict: s.Verdict,
			Conf: s.Confidence, MinConf: s.MinConfidence,
		}
		if !s.At.IsZero() {
			n.Ts = s.At.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		if len(s.MatchedRules) > 0 {
			n.Plan = s.MatchedRules
		} else if len(s.Skills) > 0 {
			n.Plan = s.Skills
		} else if len(s.Tools) > 0 {
			n.Plan = s.Tools // agent-cycle steps carry the invoked tool in the plan lane
		} else if len(s.Prompts) > 0 {
			n.Plan = s.Prompts // the propose step carries its prompt/seed/model provenance (REQ-2009)
		}
		if s.BundleVersion != "" {
			n.Hash = s.BundleVersion
		}
		for _, p := range s.PlanOps {
			n.PlanOps = append(n.PlanOps, PlanOpDTO{Op: p.Op, T: p.T})
		}
		dto.Nodes = append(dto.Nodes, n)
	}
	return dto
}

// stepState renders a step's console state honestly from its durable fields: a verify/predict step reflects
// its mechanical verdict; a boundary with no verdict yet reads "pending" (never a fabricated "ok").
func stepState(s trace.Step) string {
	switch {
	case s.Verdict == "match" || s.Verdict == "clean" || s.Verdict == "pass" || s.Verdict == "success":
		return "ok"
	case s.Verdict != "":
		return s.Verdict // deviation/partial/refuse/deny/failure/auto/approve — rendered honestly by the console
	case s.Kind == trace.StepClassify || s.Kind == trace.StepPropose || s.Kind == trace.StepScreen || s.Kind == trace.StepRag || s.Kind == trace.StepIngest || s.Kind == trace.StepCorrelate:
		return "ok" // a committed data-projection boundary (ingest/correlate/classify/propose/screen/rag), not a pending outcome
	default:
		return "pending" // an agent-cycle/policy/gate boundary with no recorded outcome yet
	}
}
