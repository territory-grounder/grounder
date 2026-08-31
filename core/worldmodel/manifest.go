// Package worldmodel is the reviewable projection of estate discovery (spec/027, epic TG-227 plane 2).
//
// THE PROBLEM IT SOLVES. TG's actuation allowlists were hand-authored: an administrator typed
// TG_ACTUATION_ALLOWED_UNITS before TG could act on anything, which made every new deployment a
// configuration PROJECT. The predecessor never asked for that — it discovered the estate and EARNED its
// scope. This package moves the AUTHORSHIP of the grant (from hand-typed env to reviewed adoption) while
// leaving the ENFORCEMENT point exactly where it is: the leaf default-deny gates in
// modules/actuation/{ssh,proxmox} are byte-untouched by this plane. Discovery proposes; only an operator's
// adopt click grants; the leaf still refuses everything it was not handed.
//
// STATE LIVES IN ONE PLACE. manifest_entry (migration 0047) is a latest-wins operational row whose HISTORY
// is the governance ledger (the policy_graduation split precedent). Status changes flow through exactly one
// audited chokepoint — Transition — cloned from core/skillstore/transition.go: an allowedTransitions map,
// mandatory rationale, ledger append BEFORE the row update, and no resurrection path.
package worldmodel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/estate"
)

// Status is the manifest entry lifecycle (REQ-2702). The zero value is StatusDraft: a row that arrives
// from discovery with no status set is a DRAFT — the state that grants nothing — never an approved one.
type Status string

const (
	// StatusDraft is discovery's output: proposed, visible, and actionable by nobody. Zero value.
	StatusDraft Status = "draft"
	// StatusApproved is the operator's grant: this entry materializes into the allowlist union.
	StatusApproved Status = "approved"
	// StatusStale marks an approved entry that discovery stopped seeing. It is NEVER auto-retired —
	// absence of evidence is not evidence of absence (REQ-2705), and silently narrowing an operator's
	// grant because a source blinked is the failure mode this status exists to avoid.
	StatusStale Status = "retired_candidate_stale"
	// StatusRetired is the operator's explicit revocation. Terminal; a rework is a NEW draft row.
	StatusRetired Status = "retired"
	// StatusRejected is the operator declining a draft. Terminal.
	StatusRejected Status = "rejected"
)

// The ledger decision strings for this plane (REQ-2702), on the ONE chain (INV-19).
const (
	DecisionDraft  = "manifest:draft"
	DecisionAdopt  = "manifest:adopt"
	DecisionReject = "manifest:reject"
	DecisionRetire = "manifest:retire"
	DecisionDrift  = "manifest:drift"
)

// allowedTransitions is the whole state machine (REQ-2702). Anything absent is refused — including
// self-transitions and every path that would resurrect a retired or rejected row (a rework is a NEW draft).
//
// STALE IS NOT A DEAD END, AND NOT AN AUTO-RETIREMENT. A stale entry is still APPROVED in substance — it
// keeps materializing — and may return to approved when discovery sees it again, or be retired by an
// explicit operator act. Nothing in the drift path may reach StatusRetired: that edge belongs to the
// operator alone.
var allowedTransitions = map[Status][]Status{
	StatusDraft:    {StatusApproved, StatusRejected},
	StatusApproved: {StatusStale, StatusRetired},
	StatusStale:    {StatusApproved, StatusRetired},
}

// ErrBadTransition refuses a status change the state machine does not declare.
var ErrBadTransition = errors.New("worldmodel: transition not allowed")

// ErrRationaleRequired refuses an unexplained state change — every grant and revocation carries a reason
// on the chain, so the ledger reads as a decision record and not a diff.
var ErrRationaleRequired = errors.New("worldmodel: rationale is required")

// ErrUnknownEntityType refuses an entry whose type is outside the estate's closed vocabulary. A typo'd or
// corrupted source must fail the load LOUDLY rather than seed a phantom target that later reads as
// operator-adopted truth (REQ-2701, the declared.go precedent).
var ErrUnknownEntityType = errors.New("worldmodel: entity type is outside the declared estate vocabulary")

func transitionAllowed(from, to Status) bool {
	for _, t := range allowedTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

func decisionFor(to Status) string {
	switch to {
	case StatusApproved:
		return DecisionAdopt
	case StatusRejected:
		return DecisionReject
	case StatusRetired:
		return DecisionRetire
	case StatusStale:
		return DecisionDrift
	default:
		return DecisionDraft
	}
}

// Entry is one discovered, reviewable estate fact: an entity the operator may adopt as an actuation target.
//
// Identity is (EntityType, Name) — the same identity the estate graph uses, so an adopted entry names
// exactly what the graph names and the two can never drift into describing different things.
type Entry struct {
	ID         int64
	EntityType estate.EntityType
	Name       string
	// Host is the machine the entity lives on (empty for host-typed entries) — the allowlist union is
	// per-target, so materialization needs to know WHERE an adopted unit runs.
	Host string
	// Source is the discovery provenance (estate.SourcePVE, the systemd source, …).
	Source estate.Source
	// Confidence is the source's table confidence. Adoption never lowers it (REQ-2706, MAX-ratchet).
	Confidence float64
	Status     Status
	// Rationale is the append-only decision log; the full history lives on the ledger.
	Rationale string
	// Approver is SERVER-DERIVED at adoption (never client-supplied) — the audit trail's subject.
	Approver        string
	LedgerSeq       int64
	FirstSeenAt     time.Time
	LastSeenAt      time.Time
	StatusChangedAt time.Time
	RetentionExpiry time.Time
}

// AllowlistKey is the identity the allowlist union matches on: the bare entity name (a systemd unit name, a
// container name, a guest name). Trimmed, never empty for a valid entry.
func (e Entry) AllowlistKey() string { return strings.TrimSpace(e.Name) }

// Ledger is the slice of audit.Ledger this plane needs — append-only governance decisions (INV-19).
type Ledger interface {
	Append(d audit.GovDecision) (audit.LedgerEntry, error)
}

// Store is the persistence surface Transition drives. The pgx implementation is compose-tested; the
// in-memory fake backs the CI oracles.
type Store interface {
	// UpdateEntry persists Status, Rationale, Approver, LedgerSeq, StatusChangedAt, Confidence.
	UpdateEntry(ctx context.Context, e Entry) error
	// ApprovedEntries returns every entry currently materializing into the allowlist union — approved
	// AND stale. Stale rows STILL materialize: discovery losing sight of a unit must not silently
	// narrow an operator's grant (REQ-2705, safe direction).
	ApprovedEntries(ctx context.Context) ([]Entry, error)
}

// clipLog bounds the append-only rationale log; the ledger holds the full history.
func clipLog(s string) string {
	const maxLog = 16384
	if len(s) <= maxLog {
		return s
	}
	return "[… clipped — full history in the governance ledger]" + s[len(s)-maxLog:]
}

// Transition is the ONLY way a manifest entry changes status (REQ-2702).
//
// The ledger entry is written BEFORE the row so a crash leaves an over-recorded ledger — a decision noted
// that did not take effect — never an unrecorded state change. An allowlist that widened with no chain
// entry is precisely the audit hole this ordering exists to make impossible.
//
// Withheld marks the decisions that do NOT widen anything (reject, retire, drift-to-stale): the chain
// records them as governance acts whose effect is to withhold or narrow, so a reader can tell a grant from
// a refusal without re-deriving it from the status.
func Transition(ctx context.Context, st Store, lg Ledger, e Entry, to Status, approver, rationale string) (Entry, error) {
	rationale = strings.TrimSpace(rationale)
	if rationale == "" {
		return Entry{}, ErrRationaleRequired
	}
	from := e.Status
	if from == "" {
		from = StatusDraft
	}
	if !transitionAllowed(from, to) {
		return Entry{}, fmt.Errorf("%w: %s -> %s (%s/%s)", ErrBadTransition, from, to, e.EntityType, e.Name)
	}
	if !KnownEntityType(e.EntityType) {
		return Entry{}, fmt.Errorf("%w: %q", ErrUnknownEntityType, e.EntityType)
	}

	// The approver rides in Reason (audit.GovDecision has no actor field — the ledger's subject is the
	// action it is bound to). The server-derived approver is also persisted on the row itself.
	reason := rationale
	if a := strings.TrimSpace(approver); a != "" {
		reason = "[" + a + "] " + rationale
	}
	entry, err := lg.Append(audit.GovDecision{
		Decision: decisionFor(to),
		Reason:   reason,
		ActionID: entryActionID(e),
		// Only adoption WIDENS. Everything else withholds or narrows.
		Withheld: to != StatusApproved,
	})
	if err != nil {
		return Entry{}, fmt.Errorf("ledger append: %w", err)
	}

	e.Status = to
	e.Rationale = clipLog(e.Rationale + "\n[" + string(to) + "] " + rationale)
	e.LedgerSeq = entry.Seq
	e.StatusChangedAt = time.Now().UTC()
	if a := strings.TrimSpace(approver); a != "" {
		e.Approver = a
	}
	if err := st.UpdateEntry(ctx, e); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// entryActionID is the stable per-entry identity the ledger binds a decision to.
func entryActionID(e Entry) string {
	return "manifest:" + string(e.EntityType) + ":" + strings.TrimSpace(e.Name)
}

// RatchetConfidence applies the MAX-ratchet (REQ-2706): a re-discovery or an adoption may only ever raise
// an entry's confidence, never lower it. Returning the max keeps the estate's sourcing policy — a
// higher-confidence source's claim is not undone by a later sighting from a weaker one.
func RatchetConfidence(current, incoming float64) float64 {
	if incoming > current {
		return incoming
	}
	return current
}

// AllowlistProvider yields the operator-granted actuation targets for one allowlist kind, composed from
// BOTH grant sources (REQ-2704). It is the seam the composition root injects at the three actuator
// constructor sites; the leaf gates that ENFORCE the allowlist are byte-untouched by this plane.
type AllowlistProvider func(ctx context.Context) []string

// AllowlistKind selects which adopted entries materialize into which leaf allowlist.
type AllowlistKind string

const (
	// KindUnit materializes into the ssh actuator's allowed-units list (systemd services).
	KindUnit AllowlistKind = "unit"
	// KindContainer materializes into the ssh actuator's allowed-containers list.
	KindContainer AllowlistKind = "container"
	// KindGuest materializes into the proxmox actuator's allowed-guests list (vm/lxc).
	KindGuest AllowlistKind = "guest"
)

// KindOf maps a manifest entry to the allowlist it materializes into, and whether it materializes at all.
// An entry whose type has no leaf to materialize into contributes to NOTHING — adopting a site or a tunnel
// grants no actuation, and the review console must say so rather than imply a grant that will not happen.
//
// EXPORTED for the review surface (spec/027 T-027-8, coordination note on T-027-4): the console computes
// "does adopting this actually grant anything?" server-side from THIS function, so the answer can never
// drift from the one UnionAllowlist uses when it materializes.
func KindOf(e Entry) (AllowlistKind, bool) {
	switch e.EntityType {
	case estate.TypeService:
		// A service entry names either a systemd unit or a container; the suffix disambiguates, and the
		// leaf allowlists are separate, so a container can never be handed to the unit gate.
		if strings.HasSuffix(e.AllowlistKey(), ".service") {
			return KindUnit, true
		}
		return KindContainer, true
	case estate.TypeVM, estate.TypeLXC:
		return KindGuest, true
	default:
		return "", false
	}
}

// UnionAllowlist composes the boot-frozen env allowlist with the adopted manifest entries of one kind
// (REQ-2704, ADR-0016 OQ-2: UNION, never DB-replaces-env).
//
// WHY UNION AND NOT REPLACE. Both sources are operator acts — one typed into config, one clicked in the
// console — and a grant must not evaporate because the other source was consulted. DB-replaces-env's
// failure mode is SILENT NARROWING on first adopt: the moment one entry is adopted, every env-granted
// target would vanish, and an operator who adopted a unit would discover they had revoked all the others.
// The union can only ever ADD to what the operator already granted.
//
// Order is env-first then adopted, each de-duplicated, so the composed list reads as
// "what you typed, plus what you clicked" — the provenance the console labels.
func UnionAllowlist(env []string, adopted []Entry, kind AllowlistKind) []string {
	out := make([]string, 0, len(env)+len(adopted))
	seen := map[string]bool{}
	add := func(v string) {
		if v = strings.TrimSpace(v); v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, e := range env {
		add(e)
	}
	for _, e := range adopted {
		if k, ok := KindOf(e); ok && k == kind {
			add(e.AllowlistKey())
		}
	}
	return out
}

// NewAllowlistProvider builds the injectable seam for one allowlist kind. A nil store, or a store error,
// yields the ENV allowlist alone — fail-closed toward the smaller, already-authored grant, never toward a
// wider one. Discovery being unavailable must never widen actuation, and must never narrow a grant the
// operator typed.
func NewAllowlistProvider(st Store, kind AllowlistKind, env []string) AllowlistProvider {
	frozen := append([]string(nil), env...)
	return func(ctx context.Context) []string {
		if st == nil {
			return frozen
		}
		adopted, err := st.ApprovedEntries(ctx)
		if err != nil {
			return frozen
		}
		return UnionAllowlist(frozen, adopted, kind)
	}
}

// knownEntityTypes mirrors core/estate's CLOSED entity vocabulary (declared.go), which is package-private
// there. It is replicated here rather than exported from estate so this plane touches only the files it
// owns — and the replication is made safe by TestVocabularyMatchesEstatesOwnRejection, which drives
// estate's PUBLIC parser and fails loudly if the two sets ever diverge (the spec/028 normalization
// precedent). A vocabulary that silently drifted would let a corrupted source seed a phantom actuation
// target that later reads as operator-adopted truth.
var knownEntityTypes = map[estate.EntityType]struct{}{
	estate.TypePhysicalHost: {}, estate.TypePVENode: {}, estate.TypeVM: {}, estate.TypeLXC: {},
	estate.TypeNetworkDevice: {}, estate.TypeStorageAppliance: {}, estate.TypeTunnel: {}, estate.TypeSite: {},
	estate.TypeService: {}, estate.TypeHost: {},
}

// KnownEntityType reports whether a type is inside the estate's declared vocabulary. Unknown ⇒ the caller
// REJECTS (loud), never coerces.
func KnownEntityType(t estate.EntityType) bool {
	_, ok := knownEntityTypes[t]
	return ok
}

// SourceConfidence returns the fixed table confidence for a discovery source, and whether the source is in
// the table at all. A source absent from the table contributes at the LEARNED cap — hard-capped below the
// 0.80 suppression cutoff (REQ-2706), so an unrecognised contributor can never outrank ground truth.
func SourceConfidence(s estate.Source) (float64, bool) {
	if c, ok := estate.SourceConfidence[s]; ok {
		return c, true
	}
	return estate.LearnedConfidence(0), false
}
