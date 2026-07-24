package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"sync"

	"github.com/territory-grounder/grounder/core/auth"
)

// The credentials read surface (spec/006 REQ-526): the console's window onto the credential engine's
// REAL persisted state — the per-source sync/drift projection (credential_sync_run, migration 0017) and
// the append-only per-target resolution audit (credential_resolution, migration 0018). It answers "which
// sources are synced and drifting?", "which targets resolved / were refused, via which source+identity?",
// and "what can TG currently reach?" (coverage derived from the resolution history).
//
// NEVER A SECRET (INV-13): every field on every DTO here is non-secret BY CONSTRUCTION — a login user, a
// connection scheme, the SCHEME of a key reference (env/file/store/vault/bao), counts, outcomes, and
// non-secret provenance. No response can carry key material, a SecretRef value/path, or a token: the pgx
// read store selects ONLY the non-secret columns (which are all these tables hold), and these structs have
// no field that could receive one. An unwired store fails closed to 503; an empty spine reports an honest
// empty result, never a fabricated row (INV-15).
//
// NOT a live probe: coverage is aggregated from the PERSISTED resolution history, never a live "can TG reach
// host X?" resolve — the SyncEngine/resolver lives in the WORKER, not the read-only grounder. A live
// resolve-probe is a documented follow-up that needs the worker-side resolver or a persisted entry set.

// CredentialSource is one sync source's latest run: its identity plane, last-synced time, the drift the last
// run observed (added/changed/removed since the prior successful sync), how many targets it currently covers,
// and the run outcome. It carries no secret — a source stores REFERENCES, never values.
type CredentialSource struct {
	SourceID       string `json:"source_id"`
	Plane          string `json:"plane"`
	LastSyncedAt   string `json:"last_synced_at,omitempty"` // RFC3339 of the last SUCCESSFUL sync; empty when never
	Added          int    `json:"added"`
	Changed        int    `json:"changed"`
	Removed        int    `json:"removed"`
	Drifted        bool   `json:"drifted"` // added+changed+removed > 0
	CoveredTargets int    `json:"covered_targets"`
	Outcome        string `json:"outcome"`       // ok / failed
	Err            string `json:"err,omitempty"` // non-secret error text; empty on ok
	// NOTE: precedence is NOT persisted (it is worker config, absent from credential_sync_run/coverage), so it
	// is honestly OMITTED here rather than fabricated. Surfacing it is a documented follow-up.
}

// CredentialSourcesPage is the read-only sync-source view the console renders, ordered plane then source_id.
type CredentialSourcesPage struct {
	Sources []CredentialSource `json:"sources"`
}

// CredentialResolution is one credential_resolution audit row: the NON-SECRET record of one per-target
// resolve. Every field is identity METADATA only — never key material, never a full SecretRef, at most a
// reference's scheme.
type CredentialResolution struct {
	Target       string   `json:"target"`
	Plane        string   `json:"plane"`
	Outcome      string   `json:"outcome"`                  // resolved / unresolved / ambiguous
	Source       string   `json:"source,omitempty"`         // winning source id; empty = native fallback / unresolved
	Native       bool     `json:"native"`                   // resolved from the native fallback store
	RuleID       string   `json:"rule_id,omitempty"`        // matched rule / winning entry id (provenance)
	ResolvedUser string   `json:"resolved_user,omitempty"`  // resolved login user (non-secret)
	Scheme       string   `json:"scheme,omitempty"`         // connection scheme (ssh/api/winrm/netconf)
	KeyRefScheme string   `json:"key_ref_scheme,omitempty"` // SCHEME of the key reference ONLY (env/file/store/vault/bao) — never its value or material
	Shadowed     []string `json:"shadowed,omitempty"`       // lower-precedence sources that also matched (non-secret provenance)
	Err          string   `json:"err,omitempty"`            // non-secret refusal text; empty on resolved
	CreatedAt    string   `json:"created_at"`               // RFC3339
}

// CredentialResolutionsPage is the read-only resolution-history view, newest first.
type CredentialResolutionsPage struct {
	Resolutions []CredentialResolution `json:"resolutions"`
}

// CredentialOutcomeCounts is the resolved/unresolved/ambiguous tally for one grouping key (a plane, or a
// source id — with '' relabelled native/unresolved) over the recent window.
type CredentialOutcomeCounts struct {
	Key        string `json:"key"`
	Resolved   int    `json:"resolved"`
	Unresolved int    `json:"unresolved"`
	Ambiguous  int    `json:"ambiguous"`
	Total      int    `json:"total"`
}

// CredentialTargetOutcome is one target's most-recent outcome in the window — the coverage frontier.
type CredentialTargetOutcome struct {
	Target    string `json:"target"`
	Outcome   string `json:"outcome"`
	Source    string `json:"source,omitempty"`
	CreatedAt string `json:"created_at"`
}

// CredentialCoverage is the coverage summary DERIVED from the recent resolution history: per-plane and
// per-source outcome tallies, plus the distinct targets most recently resolved vs refused. It answers "what
// can TG currently reach?" from REAL outcomes — never a live resolve-probe (that is a documented follow-up).
type CredentialCoverage struct {
	WindowDays     int                       `json:"window_days"`
	ByPlane        []CredentialOutcomeCounts `json:"by_plane"`
	BySource       []CredentialOutcomeCounts `json:"by_source"`
	RecentResolved []CredentialTargetOutcome `json:"recent_resolved"`
	RecentRefused  []CredentialTargetOutcome `json:"recent_refused"`
}

// CredentialsReader serves the three credential read views for the authenticated principal. All reads are
// over the real persisted projections; an empty spine returns an honest empty result. The pgx impl selects
// ONLY non-secret columns; the in-memory MemCredentialsReader drives the CI oracles (CI has no Postgres).
type CredentialsReader interface {
	CredentialSources(ctx context.Context, p auth.Principal) ([]CredentialSource, error)
	CredentialResolutions(ctx context.Context, p auth.Principal, target string, limit int) ([]CredentialResolution, error)
	CredentialCoverage(ctx context.Context, p auth.Principal) (CredentialCoverage, error)
}

// credentialsPageLimit bounds a single resolutions read; the console pages the recent tail.
const credentialsPageLimit = 200

// credentialsPageDefault is the default resolutions page when no ?limit= is supplied.
const credentialsPageDefault = 50

// credentialSourcesHandler serves GET /v1/credentials/sources. Nil reader = 503 fail-closed.
func (d Deps) credentialSourcesHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Credentials == nil {
		http.Error(w, "credentials unavailable", http.StatusServiceUnavailable)
		return
	}
	rows, err := d.Credentials.CredentialSources(r.Context(), p)
	if err != nil {
		http.Error(w, "credentials unavailable", http.StatusServiceUnavailable)
		return
	}
	if rows == nil {
		rows = []CredentialSource{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(CredentialSourcesPage{Sources: rows})
}

// credentialResolutionsHandler serves GET /v1/credentials/resolutions?target=&limit=N. The limit is capped
// at credentialsPageLimit before the read; ?target= filters to one target. Nil reader = 503 fail-closed.
func (d Deps) credentialResolutionsHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Credentials == nil {
		http.Error(w, "credentials unavailable", http.StatusServiceUnavailable)
		return
	}
	limit := credentialsPageDefault
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > credentialsPageLimit {
		limit = credentialsPageLimit
	}
	target := r.URL.Query().Get("target")
	rows, err := d.Credentials.CredentialResolutions(r.Context(), p, target, limit)
	if err != nil {
		http.Error(w, "credentials unavailable", http.StatusServiceUnavailable)
		return
	}
	if rows == nil {
		rows = []CredentialResolution{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(CredentialResolutionsPage{Resolutions: rows})
}

// credentialCoverageHandler serves GET /v1/credentials/coverage. Nil reader = 503 fail-closed.
func (d Deps) credentialCoverageHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Credentials == nil {
		http.Error(w, "credentials unavailable", http.StatusServiceUnavailable)
		return
	}
	cov, err := d.Credentials.CredentialCoverage(r.Context(), p)
	if err != nil {
		http.Error(w, "credentials unavailable", http.StatusServiceUnavailable)
		return
	}
	if cov.ByPlane == nil {
		cov.ByPlane = []CredentialOutcomeCounts{}
	}
	if cov.BySource == nil {
		cov.BySource = []CredentialOutcomeCounts{}
	}
	if cov.RecentResolved == nil {
		cov.RecentResolved = []CredentialTargetOutcome{}
	}
	if cov.RecentRefused == nil {
		cov.RecentRefused = []CredentialTargetOutcome{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cov)
}

// MemCredentialsReader is the in-memory CredentialsReader twin for the CI oracles (no Postgres). It holds
// canned, NON-SECRET DTOs and applies the ?target= filter + the (already-capped) limit exactly as the pgx
// store does, and records the target/limit it last received so a test can assert the handler capped/forwarded
// them. It is secret-free by construction — its only inputs are the non-secret DTOs. Concurrency-safe.
type MemCredentialsReader struct {
	mu          sync.Mutex
	Sources     []CredentialSource
	Resolutions []CredentialResolution // newest-first
	Coverage    CredentialCoverage
	Err         error // when set, every method returns it (drives the 503 oracle)

	// spies for the resolutions filter/cap oracle.
	LastTarget string
	LastLimit  int
}

// CredentialSources returns the canned sources, ordered plane then source_id (matching the pgx ORDER BY).
func (m *MemCredentialsReader) CredentialSources(_ context.Context, _ auth.Principal) ([]CredentialSource, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return nil, m.Err
	}
	out := append([]CredentialSource(nil), m.Sources...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Plane != out[j].Plane {
			return out[i].Plane < out[j].Plane
		}
		return out[i].SourceID < out[j].SourceID
	})
	return out, nil
}

// CredentialResolutions applies the target filter then the limit, newest-first, recording both for the oracle.
func (m *MemCredentialsReader) CredentialResolutions(_ context.Context, _ auth.Principal, target string, limit int) ([]CredentialResolution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LastTarget = target
	m.LastLimit = limit
	if m.Err != nil {
		return nil, m.Err
	}
	out := make([]CredentialResolution, 0, len(m.Resolutions))
	for _, rr := range m.Resolutions {
		if target != "" && rr.Target != target {
			continue
		}
		out = append(out, rr)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// CredentialCoverage returns the canned coverage summary.
func (m *MemCredentialsReader) CredentialCoverage(_ context.Context, _ auth.Principal) (CredentialCoverage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return CredentialCoverage{}, m.Err
	}
	return m.Coverage, nil
}

// compile-time proof the in-memory twin satisfies the reader interface.
var _ CredentialsReader = (*MemCredentialsReader)(nil)
