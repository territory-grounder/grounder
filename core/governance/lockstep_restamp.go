package governance

// Re-stamp authorization for the content-aware spec↔code lockstep manifest.
//
// Provenance: [F] spec/007 · [R] paradigm-rule 4 · [O] REQ-703 (a re-stamp is accepted ONLY through an
// authorized, RBAC-gated, audited approval appended to the governance ledger — never a host-local edit),
// INV-19 (tamper-evident ledger), INV-22 (no governed file drifts unbound).
//
// The lockstep gate (tools/specvalidate) makes a host-local hash edit fail CI as spec drift. This file is
// the other half: the ONLY legitimate way to MOVE a recorded hash. A hash may move only inside an
// authorized approval, raised by a permitted spec-owner role, when the owning spec is updated in the SAME
// change — and every acceptance leaves an immutable, RBAC-attributed record on the governance ledger. The
// RBAC policy is an injected interface (fail closed: a nil authority, or a role the policy does not
// permit, denies), so the mechanical acceptance rule lives here while the policy plugs in without a
// dependency cycle.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/audit"
)

// RestampAuthority is the RBAC seam that decides whether an actor role may authorize a re-stamp of a
// given spec. It is an interface so the policy layer plugs in without this package depending on it. A nil
// authority denies (fail closed); an unpermitted role denies. [O] REQ-703, paradigm-rule 4.
type RestampAuthority interface {
	// MayRestamp reports whether actorRole is permitted to authorize a re-stamp of owningSpec.
	MayRestamp(actorRole, owningSpec string) bool
}

// RestampApproval is an RBAC-gated request to accept re-stamped content hashes for one owning spec. A
// re-stamp is legitimate ONLY inside such an approval: raised by a permitted spec-owner role, and only
// when the owning spec is updated in the SAME change (a hash may move only when its spec moves with it).
type RestampApproval struct {
	ActorRole    string   // the role authorizing the re-stamp; must be permitted by the authority
	OwningSpec   string   // the spec whose governed files are being re-stamped
	ChangedPaths []string // the governed files whose recorded hashes change
	SpecUpdated  bool     // whether OwningSpec was updated in the same change (REQ-703)
}

// ErrUnauthorizedRestamp rejects a re-stamp not carried by an authorized, audited approval — a host-local
// manifest edit is spec drift, never a legitimate re-stamp. Fail closed. [O] REQ-703, INV-22.
var ErrUnauthorizedRestamp = errors.New("governance: re-stamp rejected — not an authorized, audited approval (spec drift)")

// restampActionID binds the re-stamp record to the exact (spec, changed-paths) tuple it authorizes, so the
// ledger entry is content-addressed: a different owning spec or a different set of paths yields a
// different action id (INV-07). Paths are sorted so the id is order-independent.
func restampActionID(a RestampApproval) string {
	paths := append([]string(nil), a.ChangedPaths...)
	sort.Strings(paths)
	sum := sha256.Sum256([]byte(a.OwningSpec + "\x00" + strings.Join(paths, "\x00")))
	return "lockstep-restamp:" + hex.EncodeToString(sum[:])[:16]
}

// AuthorizeRestamp is the SOLE path by which re-stamped content hashes may be accepted. It accepts ONLY
// through an authorized, RBAC-gated approval whose owning spec is updated in the same change, and appends
// an immutable, RBAC-attributed re-stamp record — carrying the actor role, the changed paths, and the
// owning spec — to the tamper-evident governance ledger (INV-19). Any missing element (nil ledger, nil
// authority, empty spec/paths, the owning spec not updated, or a role the policy does not permit) is
// rejected as spec drift with NO ledger record written. There is no host-local path to acceptance.
// [O] REQ-703, INV-19/22, paradigm-rule 4.
func AuthorizeRestamp(authority RestampAuthority, appr RestampApproval, ledger *audit.Ledger) error {
	if ledger == nil || authority == nil {
		return ErrUnauthorizedRestamp
	}
	if appr.OwningSpec == "" || len(appr.ChangedPaths) == 0 {
		return ErrUnauthorizedRestamp
	}
	if !appr.SpecUpdated {
		// A recorded hash may move only when its owning spec moves with it, in the same change (REQ-703).
		return ErrUnauthorizedRestamp
	}
	if !authority.MayRestamp(appr.ActorRole, appr.OwningSpec) {
		return ErrUnauthorizedRestamp
	}
	// Authorized: append the immutable re-stamp record. The reason carries the actor role, owning spec, and
	// changed paths so the re-stamp is fully reconstructable from the ledger alone (REQ-703).
	paths := append([]string(nil), appr.ChangedPaths...)
	sort.Strings(paths)
	if _, err := ledger.Append(audit.GovDecision{
		Decision: "lockstep:restamp",
		Reason:   "role=" + appr.ActorRole + " spec=" + appr.OwningSpec + " paths=" + strings.Join(paths, ","),
		ActionID: restampActionID(appr),
		Withheld: false, // an authorized acceptance, not a withholding
	}); err != nil {
		return err
	}
	return nil
}
