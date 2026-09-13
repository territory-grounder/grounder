package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/worldmodel"
)

// The world-model manifest surface (spec/027 REQ-2703, epic TG-227 plane 2): the REVIEW-not-AUTHOR flow.
//
// Discovery drafts what the estate actually runs; this surface is where a human looks at those drafts and
// grants — or refuses — each one. The read half is AuthReadOnly and shows every entry with its provenance,
// confidence, and status. The write half is three CLOSED verbs (adopt/reject/retire) behind an
// operator session, each carrying a mandatory rationale and executing through worldmodel.Transition —
// the one audited chokepoint that appends the ledger row BEFORE the row update.
//
// PARADIGM RULE 9 (CONSTITUTION.md:237) IS THE SHAPE OF THIS FILE: estate knowledge is self-populating,
// so there is NO endpoint here for hand-authoring a manifest entry. An operator can approve, refuse, or
// revoke what discovery found; they cannot type a new target into existence. That is the whole difference
// between "the admin reviews" and "the admin authors", and it is enforced by the absence of a create route,
// not by a comment: the verb table below is closed and contains no create.
//
// Hardening is the vote.go kit, for the same reason it exists there — this lane widens actuation:
// same-origin enforcement, a per-caller rate limit, a server-derived approver (never a client claim),
// honest 409-vs-503, and bounded request bodies.

// ManifestEntryView is one reviewable estate fact. Every field is persisted, non-secret data (INV-13):
// an entity type, a name, the host it runs on, its discovery provenance, and its lifecycle.
type ManifestEntryView struct {
	ID         int64   `json:"id"`
	EntityType string  `json:"entity_type"`
	Name       string  `json:"name"`
	Host       string  `json:"host,omitempty"`
	Source     string  `json:"source"`
	Confidence float64 `json:"confidence"`
	Status     string  `json:"status"`
	// Approver is the SERVER-DERIVED operator who last moved this row — the audit trail's subject.
	Approver  string `json:"approver,omitempty"`
	LedgerSeq int64  `json:"ledger_seq,omitempty"`
	// Materializes reports whether this entry is currently contributing to an allowlist — the honest
	// answer to "does adopting this actually grant anything?". Computed server-side from the entry's
	// kind: a site or a tunnel materializes into no leaf, so adopting one grants nothing and the console
	// must not imply otherwise.
	Materializes bool `json:"materializes"`
	// AllowlistKind names WHICH leaf allowlist this entry feeds (unit/container/guest), empty when none.
	AllowlistKind string `json:"allowlist_kind,omitempty"`
	FirstSeenAt   string `json:"first_seen_at,omitempty"`
	LastSeenAt    string `json:"last_seen_at,omitempty"`
	// CallerCanAct is SERVER-computed: only an authenticated operator session can reach the write verbs,
	// so a machine principal sees the queue read-only and the console never renders a control that
	// would refuse. The console keys its buttons off this, never off its own guess.
	CallerCanAct bool `json:"caller_can_act"`
}

// ManifestPage is the review surface: the full reviewable set plus the honest counts the rail badge shows.
type ManifestPage struct {
	Entries []ManifestEntryView `json:"entries"`
	// Drafts is the REAL number of entries awaiting review — the badge's number, never len(page).
	Drafts int `json:"drafts"`
	Total  int `json:"total"`
	// CallerCanAct mirrors the per-row flag for the page chrome (the "sign in to adopt" hint).
	CallerCanAct bool `json:"caller_can_act"`
}

// ManifestTransitionOutcome is the synchronous result the console re-renders from.
type ManifestTransitionOutcome struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	LedgerSeq int64  `json:"ledger_seq"`
}

// ManifestReader is the read surface. Nil ⇒ 503 fail-closed (never a fabricated row, INV-15).
type ManifestReader interface {
	// ManifestEntries returns every reviewable entry — drafts and decided alike, newest activity first.
	ManifestEntries(ctx context.Context, p auth.Principal, limit int) (entries []worldmodel.Entry, drafts, total int, err error)
}

// ManifestWriter executes an operator's verb through the worker — the ledger's single writer — so the
// ledger-before-row ordering happens exactly once, in worldmodel.Transition, and this surface can never
// become a second status writer.
type ManifestWriter interface {
	Transition(ctx context.Context, id int64, to worldmodel.Status, rationale, approver string) (ManifestTransitionOutcome, error)
}

// manifestVerbs is the CLOSED verb table (REQ-2703): route text is never a status. There is deliberately
// no "create" and no "draft" verb — discovery is the only author of rows (paradigm rule 9), and an
// operator who could POST a draft could hand-author an actuation target, which is precisely the
// configuration-project failure mode this whole plane exists to remove.
var manifestVerbs = map[string]worldmodel.Status{
	"adopt":  worldmodel.StatusApproved,
	"reject": worldmodel.StatusRejected,
	"retire": worldmodel.StatusRetired,
}

// manifestPageLimit bounds one read; the console reviews the reviewable set, not the archive.
const manifestPageLimit = 500

// manifestLimits is the per-operator write rate limit (defense in depth, the vote.go precedent): a runaway
// client cannot mass-adopt the estate faster than a human could have reviewed it. It is its OWN limiter
// instance — sharing the vote lane's window would let approvals and adoptions starve each other — and
// reuses that lane's per-minute budget, which is already sized to "a human clicking deliberately".
var manifestLimits voteLimiter

// manifestHandler serves GET /v1/manifest — the review surface.
func (d Deps) manifestHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Manifest == nil {
		http.Error(w, "manifest surface unavailable", http.StatusServiceUnavailable)
		return
	}
	limit := manifestPageLimit
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < manifestPageLimit {
			limit = n
		}
	}
	rows, drafts, total, err := d.Manifest.ManifestEntries(r.Context(), p, limit)
	if err != nil {
		http.Error(w, "manifest surface unavailable", http.StatusServiceUnavailable)
		return
	}
	// Only an authenticated operator session can reach the write verbs (they are AuthSession); a machine
	// principal sees the review queue read-only.
	canAct := strings.HasPrefix(p.SourceID, "operator:")
	views := make([]ManifestEntryView, 0, len(rows))
	for _, e := range rows {
		kind, materializes := worldmodel.KindOf(e)
		v := ManifestEntryView{
			ID:            e.ID,
			EntityType:    string(e.EntityType),
			Name:          e.Name,
			Host:          e.Host,
			Source:        string(e.Source),
			Confidence:    e.Confidence,
			Status:        string(e.Status),
			Approver:      e.Approver,
			LedgerSeq:     e.LedgerSeq,
			Materializes:  materializes,
			AllowlistKind: string(kind),
			CallerCanAct:  canAct,
		}
		if !e.FirstSeenAt.IsZero() {
			v.FirstSeenAt = e.FirstSeenAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		if !e.LastSeenAt.IsZero() {
			v.LastSeenAt = e.LastSeenAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		views = append(views, v)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ManifestPage{
		Entries: views, Drafts: drafts, Total: total, CallerCanAct: canAct,
	})
}

func (d Deps) manifestWriteGuard(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if d.ManifestWrite == nil {
		http.Error(w, "manifest write path unavailable", http.StatusServiceUnavailable)
		return false
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin write rejected", http.StatusForbidden)
		return false
	}
	return true
}

// manifestWriteErr maps the state machine's refusals to honest status codes. The distinction that matters:
// a 409 means the decision is genuinely not available from this state (already decided, terminal row); a
// 503 means the write path itself is unwell and the caller should retry. Collapsing the two would teach an
// operator that a retryable failure is a permanent refusal, or worse, the reverse.
func manifestWriteErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, worldmodel.ErrRationaleRequired):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, worldmodel.ErrUnknownEntityType):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, worldmodel.ErrBadTransition):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrManifestEntryNotFound):
		http.Error(w, "unknown manifest entry", http.StatusNotFound)
	default:
		http.Error(w, "manifest write failed — retry", http.StatusServiceUnavailable)
	}
}

// ErrManifestEntryNotFound is the writer's signal that the id names no reviewable row.
var ErrManifestEntryNotFound = errors.New("httpapi: unknown manifest entry")

// manifestTransitionHandler serves POST /v1/manifest/entries/{id}/{verb} for the closed verb table.
func (d Deps) manifestTransitionHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !d.manifestWriteGuard(w, r) {
		return
	}
	// The approver is the AUTHENTICATED principal, server-derived. A body-supplied approver would make
	// the audit trail a client claim.
	approver := operatorOf(p)
	if !manifestLimits.allow(approver, time.Now()) {
		http.Error(w, "manifest write rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "entry id required", http.StatusBadRequest)
		return
	}
	to, ok := manifestVerbs[chi.URLParam(r, "verb")]
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
	// Refused at the surface for a fast, legible 400 — and refused AGAIN in worldmodel.Transition, which
	// is the authority. Two layers because this one can be bypassed by a future caller; that one cannot.
	if strings.TrimSpace(req.Rationale) == "" {
		http.Error(w, "rationale required — every grant and revocation states why", http.StatusBadRequest)
		return
	}
	out, err := d.ManifestWrite.Transition(r.Context(), id, to, strings.TrimSpace(req.Rationale), approver)
	if err != nil {
		manifestWriteErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// ManifestHandlerForAcceptance / ManifestTransitionHandlerForAcceptance expose the unexported handlers to
// the spec/027 acceptance oracle (an external package driving the REAL handlers). They add no behavior —
// the oracle must exercise exactly what production serves.
func (d Deps) ManifestHandlerForAcceptance(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	d.manifestHandler(w, r, p)
}

// ManifestTransitionHandlerForAcceptance — see ManifestHandlerForAcceptance.
func (d Deps) ManifestTransitionHandlerForAcceptance(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	d.manifestTransitionHandler(w, r, p)
}
