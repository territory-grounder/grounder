package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"

	"github.com/territory-grounder/grounder/core/auth"
)

// The Actuation Regime Engine read surface (spec/017 T-017-7, REQ-1716): the console's window onto the
// engine's REAL persisted state (migration 0020) — the append-only per-selection resolution audit
// (regime_resolution: target → regime → lane), the per-launch actuation audit (regime_actuation), the
// per-completed-verify deferred_verdict audit, and a per-lane coverage roll-up derived from them. It answers
// "which lane did the engine route this target to?", "what did it launch and under which op-class?", "how did
// the deferred verify resolve?", and "which lanes carry any traffic?".
//
// NEVER A SECRET (INV-13): every field on every DTO here is non-secret BY CONSTRUCTION — target labels,
// regime/lane slugs, rule ids, op-classes, AWX job ids, verdict/graduation slugs, and the AWX token as a
// SecretRef REFERENCE (token_ref, e.g. "store:awx.token") — never a token value or a secret extra_var. The
// pgx read store selects ONLY the non-secret columns the 0020 tables hold, and these structs have no field
// that could receive a secret. An unwired store fails closed to 503; at mode Shadow the tables are EMPTY by
// design (no resolution/launch/verdict occurs before the flip), so the surface reports an honest empty result,
// never a fabricated row (INV-15).
//
// Read-only: this surface selects nothing, launches nothing, and edits no allowlist (the console
// template-allowlist editor is a SEPARATE follow-on). Mutation stays OFF.

// RegimeResolution is one regime_resolution audit row: the NON-SECRET record of one lane selection. A refusal
// carries an empty regime/lane (fail closed); a resolved row names both.
type RegimeResolution struct {
	Target    string `json:"target"`
	Regime    string `json:"regime,omitempty"`
	Lane      string `json:"lane,omitempty"`
	RuleID    string `json:"rule_id,omitempty"` // matched rule id; empty = operator default lane
	Outcome   string `json:"outcome"`           // resolved / refused
	CreatedAt string `json:"created_at"`        // RFC3339
}

// RegimeActuation is one regime_actuation audit row: the NON-SECRET record of one job launch. TokenRef is a
// SecretRef REFERENCE, never a value (INV-13).
type RegimeActuation struct {
	ActionID      string `json:"action_id"`
	Lane          string `json:"lane"`
	JobTemplateID string `json:"job_template_id,omitempty"`
	OpClass       string `json:"op_class,omitempty"`
	JobID         string `json:"job_id,omitempty"`
	TokenRef      string `json:"token_ref,omitempty"` // a SecretRef reference (env:/file:/store:), never a value
	CreatedAt     string `json:"created_at"`          // RFC3339
}

// RegimeDeferredVerdict is one deferred_verdict audit row: the NON-SECRET outcome of one completed deferred
// verify (the launch was a prediction; this is the terminal-status mechanical verdict + graduation outcome).
type RegimeDeferredVerdict struct {
	ActionID   string `json:"action_id"`
	JobID      string `json:"job_id,omitempty"`
	Status     string `json:"status"`     // terminal AWX status: successful / failed / error / canceled
	Verdict    string `json:"verdict"`    // match / deviation / unverified
	Graduation string `json:"graduation"` // verified_clean / deviated / no_credit
	CreatedAt  string `json:"created_at"` // RFC3339
}

// RegimeLaneCoverage is one lane's roll-up: how many resolved selections and launches it carries. At Shadow
// every count is zero (no lane has been routed through yet).
type RegimeLaneCoverage struct {
	Lane        string `json:"lane"`
	Resolutions int    `json:"resolutions"`
	Actuations  int    `json:"actuations"`
}

// RegimePage is the aggregated regime read view for the console: the recent resolution / actuation /
// deferred-verdict tails plus the per-lane coverage roll-up.
type RegimePage struct {
	Resolutions      []RegimeResolution      `json:"resolutions"`
	Actuations       []RegimeActuation       `json:"actuations"`
	DeferredVerdicts []RegimeDeferredVerdict `json:"deferred_verdicts"`
	LaneCoverage     []RegimeLaneCoverage    `json:"lane_coverage"`
}

// RegimeReader serves the regime read views for the authenticated principal. All reads are over the real
// persisted 0020 projections; an empty spine returns an honest empty result. The pgx-backed impl selects ONLY
// non-secret columns; the in-memory MemRegimeReader drives the CI oracles (CI has no Postgres).
type RegimeReader interface {
	RegimeResolutions(ctx context.Context, p auth.Principal, limit int) ([]RegimeResolution, error)
	RegimeActuations(ctx context.Context, p auth.Principal, limit int) ([]RegimeActuation, error)
	RegimeDeferredVerdicts(ctx context.Context, p auth.Principal, limit int) ([]RegimeDeferredVerdict, error)
	RegimeLaneCoverage(ctx context.Context, p auth.Principal) ([]RegimeLaneCoverage, error)
}

// regimePageLimit bounds a single read; the console pages the recent tail.
const regimePageLimit = 200

// regimePageDefault is the default page when no ?limit= is supplied.
const regimePageDefault = 50

// regimeHandler serves GET /v1/regime?limit=N — the aggregated regime read view (resolutions, actuations,
// deferred verdicts, lane coverage). The limit is capped at regimePageLimit before the read. Nil reader =
// 503 fail-closed.
func (d Deps) regimeHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Regime == nil {
		http.Error(w, "regime unavailable", http.StatusServiceUnavailable)
		return
	}
	limit := regimePageDefault
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > regimePageLimit {
		limit = regimePageLimit
	}
	ctx := r.Context()
	resolutions, err := d.Regime.RegimeResolutions(ctx, p, limit)
	if err != nil {
		http.Error(w, "regime unavailable", http.StatusServiceUnavailable)
		return
	}
	actuations, err := d.Regime.RegimeActuations(ctx, p, limit)
	if err != nil {
		http.Error(w, "regime unavailable", http.StatusServiceUnavailable)
		return
	}
	verdicts, err := d.Regime.RegimeDeferredVerdicts(ctx, p, limit)
	if err != nil {
		http.Error(w, "regime unavailable", http.StatusServiceUnavailable)
		return
	}
	coverage, err := d.Regime.RegimeLaneCoverage(ctx, p)
	if err != nil {
		http.Error(w, "regime unavailable", http.StatusServiceUnavailable)
		return
	}
	page := RegimePage{
		Resolutions:      resolutions,
		Actuations:       actuations,
		DeferredVerdicts: verdicts,
		LaneCoverage:     coverage,
	}
	if page.Resolutions == nil {
		page.Resolutions = []RegimeResolution{}
	}
	if page.Actuations == nil {
		page.Actuations = []RegimeActuation{}
	}
	if page.DeferredVerdicts == nil {
		page.DeferredVerdicts = []RegimeDeferredVerdict{}
	}
	if page.LaneCoverage == nil {
		page.LaneCoverage = []RegimeLaneCoverage{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(page)
}

// MemRegimeReader is the in-memory RegimeReader twin for the CI oracles (no Postgres). It holds canned,
// NON-SECRET DTOs and applies the (already-capped) limit exactly as the pgx store does, recording the limit it
// last received so a test can assert the handler capped/forwarded it. Secret-free by construction.
// Concurrency-safe.
type MemRegimeReader struct {
	mu               sync.Mutex
	Resolutions      []RegimeResolution
	Actuations       []RegimeActuation
	DeferredVerdicts []RegimeDeferredVerdict
	Coverage         []RegimeLaneCoverage
	Err              error // when set, every method returns it (drives the 503 oracle)
	LastLimit        int   // spy for the cap oracle
}

// RegimeResolutions returns the canned resolutions, capped at limit; records the limit for the oracle.
func (m *MemRegimeReader) RegimeResolutions(_ context.Context, _ auth.Principal, limit int) ([]RegimeResolution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LastLimit = limit
	if m.Err != nil {
		return nil, m.Err
	}
	return capResolutions(m.Resolutions, limit), nil
}

// RegimeActuations returns the canned actuations, capped at limit.
func (m *MemRegimeReader) RegimeActuations(_ context.Context, _ auth.Principal, limit int) ([]RegimeActuation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return nil, m.Err
	}
	out := make([]RegimeActuation, 0, len(m.Actuations))
	for _, a := range m.Actuations {
		out = append(out, a)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// RegimeDeferredVerdicts returns the canned deferred verdicts, capped at limit.
func (m *MemRegimeReader) RegimeDeferredVerdicts(_ context.Context, _ auth.Principal, limit int) ([]RegimeDeferredVerdict, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return nil, m.Err
	}
	out := make([]RegimeDeferredVerdict, 0, len(m.DeferredVerdicts))
	for _, v := range m.DeferredVerdicts {
		out = append(out, v)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// RegimeLaneCoverage returns the canned lane-coverage roll-up.
func (m *MemRegimeReader) RegimeLaneCoverage(_ context.Context, _ auth.Principal) ([]RegimeLaneCoverage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return nil, m.Err
	}
	return append([]RegimeLaneCoverage(nil), m.Coverage...), nil
}

func capResolutions(in []RegimeResolution, limit int) []RegimeResolution {
	out := make([]RegimeResolution, 0, len(in))
	for _, r := range in {
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// compile-time proof the in-memory twin satisfies the reader interface.
var _ RegimeReader = (*MemRegimeReader)(nil)
