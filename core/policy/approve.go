package policy

import (
	"context"
	"strings"

	"github.com/territory-grounder/grounder/core/credential"
)

// ---------------------------------------------------------------------------------------------------------
// approve_by ENFORCEMENT on the vote path (spec/015 task T-015-9, REQ-1516).
//
// REQ-1516: the engine SHALL admit an approval vote on /v1/vote ONLY WHEN the voting principal is a MEMBER of
// the pending decision's approve_by set — a vote from a non-member SHALL be rejected. MayApprove is the pure,
// injectable check that decides this. It is the SEAM the /v1/vote handler calls: the handler authenticates
// the acting principal (INV-13) and loads the pending decision's approve_by (its Rule.ApproveBy, INV-12),
// then asks MayApprove whether that actor may cast the approving vote. This leaf owns the check + the seam;
// wiring the actual HTTP handler is the integration point (T-015-9 owns no http file).
//
// FAIL CLOSED throughout: a resolver error, a human-plane backend error, an empty approve_by, or simply
// no-match all DENY. There is NEVER a default "anyone may approve" — mirroring the credential human plane
// (core/credential/plane.go, REQ-1602/1611).
// ---------------------------------------------------------------------------------------------------------

// ApproveDecision is the non-secret audit record of one vote-admission check: the acting actor, whether the
// vote was admitted, the approve_by entry that admitted it (empty when denied), and a human-readable reason.
// It carries NO secret material and is safe to append to the governance ledger.
type ApproveDecision struct {
	Actor        string // the voting actor id (non-secret)
	Allowed      bool   // true iff the vote is admitted
	MatchedEntry string // the approve_by entry that admitted the vote ("" when denied)
	Reason       string // non-secret, audit-friendly explanation
}

// MayApprove reports whether actorID may cast the approving vote for a decision whose rule names approveBy,
// resolving the actor through resolver (the shared local + human-plane identity). It returns true ONLY WHEN
// the actor's principal id is named in approve_by (a `user:` entry) OR the actor is a member of a group named
// in approve_by (a `group:` entry) — resolved locally via Principal.Groups and, additionally, via the
// credential human plane (REQ-1516).
//
// Empty approve_by → DENY. REQ-1516 admits a vote ONLY WHEN the voter is a MEMBER of the approve_by set; an
// empty set has no members, so no one may approve. This is the SAFER of the two readings and is consistent
// with the human plane's fail-closed rule that there is never a default "anyone may approve"
// (core/credential/plane.go, REQ-1602/1611).
//
// A nil resolver, a resolver error, or a human-plane backend error all DENY (fail closed).
func MayApprove(ctx context.Context, resolver PrincipalResolver, actorID string, approveBy []string) (bool, ApproveDecision) {
	dec := ApproveDecision{Actor: strings.TrimSpace(actorID)}

	// Empty approve_by (no non-blank entry) → fail closed: no principal is authorized to approve.
	hasEntry := false
	for _, e := range approveBy {
		if strings.TrimSpace(e) != "" {
			hasEntry = true
			break
		}
	}
	if !hasEntry {
		dec.Reason = "empty approve_by — no principal is authorized to approve (fail closed, REQ-1516)"
		return false, dec
	}

	if resolver == nil {
		dec.Reason = "no principal resolver configured — cannot verify membership (fail closed)"
		return false, dec
	}
	p, err := resolver.Resolve(ctx, actorID)
	if err != nil {
		dec.Reason = "principal resolution failed — deny (fail closed)"
		return false, dec
	}

	gm, hasHuman := resolver.(groupMemberResolver)

	for _, raw := range approveBy {
		ref, ok := parseApproveByEntry(raw)
		if !ok {
			continue
		}
		// A `user:` entry (or an unprefixed entry) matches when the actor's id is named directly.
		if ref.kind != credential.PrincipalGroup && principalIDMatches(p.ID, ref.name) {
			return admit(&dec, raw, "actor id is named in approve_by")
		}
		// A `group:` entry (or an unprefixed entry) matches when the actor is a member of the named group —
		// first via the local registry, then via the SHARED credential human plane.
		if ref.kind != credential.PrincipalUser {
			if hasGroupMembership(p.Groups, ref.name) {
				return admit(&dec, raw, "actor is a member of an approve_by group (local registry)")
			}
			if hasHuman {
				member, herr := gm.groupMember(ctx, actorID, ref.name)
				if herr != nil {
					dec.Reason = "human-plane group resolution failed — deny (fail closed)"
					return false, dec
				}
				if member {
					return admit(&dec, raw, "actor is a member of an approve_by group (credential human plane)")
				}
			}
		}
	}

	dec.Reason = "actor is not named in approve_by and is a member of no approve_by group (fail closed, REQ-1516)"
	return false, dec
}

// VoteAdmission is the injectable seam the /v1/vote handler holds: a resolver bound once at wiring time, so
// the handler asks Admit(actor, pendingDecision.ApproveBy) without knowing whether identity is local or
// federated. It is a thin adapter over MayApprove.
type VoteAdmission struct {
	Resolver PrincipalResolver
}

// Admit runs the approve_by membership check for a vote. See MayApprove.
func (v VoteAdmission) Admit(ctx context.Context, actorID string, approveBy []string) (bool, ApproveDecision) {
	return MayApprove(ctx, v.Resolver, actorID, approveBy)
}

// approverRef is a parsed approve_by entry: a kind (user / group, or "" for an unprefixed entry that may
// match either) and the bare principal / group name.
type approverRef struct {
	kind credential.PrincipalKind // PrincipalUser | PrincipalGroup | "" (unprefixed → match id OR group)
	name string
}

// parseApproveByEntry parses one approve_by entry of the design.md grammar `{user:* | group:*}`. An
// unprefixed entry is accepted and matched against BOTH the actor id and the actor's groups (lenient but
// still fail-closed — it only ever grants a real match). A blank or prefix-only entry is skipped.
func parseApproveByEntry(raw string) (approverRef, bool) {
	s := strings.TrimSpace(raw)
	switch {
	case s == "":
		return approverRef{}, false
	case strings.HasPrefix(s, "user:"):
		name := strings.TrimSpace(strings.TrimPrefix(s, "user:"))
		if name == "" {
			return approverRef{}, false
		}
		return approverRef{kind: credential.PrincipalUser, name: name}, true
	case strings.HasPrefix(s, "group:"):
		name := strings.TrimSpace(strings.TrimPrefix(s, "group:"))
		if name == "" {
			return approverRef{}, false
		}
		return approverRef{kind: credential.PrincipalGroup, name: name}, true
	default:
		return approverRef{kind: "", name: s}, true
	}
}

// principalIDMatches compares an actor id to a `user:` target, case-insensitively (trimmed) — the same
// case-fold discipline the credential human plane uses for approver names.
func principalIDMatches(id, want string) bool {
	return strings.EqualFold(strings.TrimSpace(id), strings.TrimSpace(want))
}

// hasGroupMembership reports whether the actor's (already-normalized) groups include want, case-insensitively
// (trimmed) — matching the credential human plane's group comparison (plane.go: approverMatches / EqualFold).
func hasGroupMembership(groups []string, want string) bool {
	w := strings.TrimSpace(want)
	for _, g := range groups {
		if strings.EqualFold(strings.TrimSpace(g), w) {
			return true
		}
	}
	return false
}

// admit stamps the decision as allowed with the matching approve_by entry and reason, and returns it.
func admit(dec *ApproveDecision, entry, reason string) (bool, ApproveDecision) {
	dec.Allowed = true
	dec.MatchedEntry = strings.TrimSpace(entry)
	dec.Reason = reason
	return true, *dec
}
