package actorevidence

import "context"

// SanctionResolver is the identity/auth enrichment seam (spec/023 REQ-2315..2319) — deliberately NOT a
// Reader. An action-evidence Reader answers "actor X did action Y on target Z at time T"; an identity
// resolver answers a different question — "is principal X an enabled, non-service member of the sanctioned
// admin group, or is it a disabled/locked credential?" — which names no target, action, or time and so can
// never be an Evidence record or by itself set a taxonomy (REQ-2315 preserves REQ-2312).
//
// It is consulted ONLY for actors already named by admissible action-evidence, and its result only REFINES
// the classification of those actors in the restrictive direction (REQ-2317/2318): a Confirmed live admin is
// promoted from a would-be suspicious reading to attributed-authorized (stand-down); a Disabled principal is
// demoted from attributed-authorized to attributed-suspicious (security-escalate — a decommissioned admin
// credential in use is a compromise indicator). Both refinements fire only on FRESH POSITIVE facts.
//
// The seam is read-only by construction and ADVISORY / fail-open (REQ-2319): an error or timeout yields
// empty facts, leaving the classification byte-identical to the static Phase-1 ladder — a dead resolver can
// neither promote (grant a stand-down) nor demote (mint suspicious).
type SanctionResolver interface {
	// Dimension identifies the identity provider ("ldap").
	Dimension() string
	// ReadOnly is always true — the seam is read-only by construction.
	ReadOnly() bool
	// Resolve returns, for the DISTINCT actors already named by admissible action-evidence in one domain,
	// which are affirmatively confirmed and which are affirmatively disabled. sanctionedGroups are the
	// domain's configured sanctioned admin group names (loadable rules-as-data, REQ-2308 / REQ-2317) whose
	// enabled non-service members are Confirmed. An error is advisory (REQ-2319): the caller treats the
	// result as absent and neither promotes nor demotes.
	Resolve(ctx context.Context, domain string, actors, sanctionedGroups []string) (SanctionFacts, error)
}

// SanctionFacts is the resolver's minimized output (REQ-2320): only the actor principals in each affirmative
// bucket, plus provider/domain-only warnings. It carries NO group membership detail, NO account attributes,
// and never the raw directory entry — the classification signal records only the resolved taxonomy, never
// the principal.
type SanctionFacts struct {
	// Confirmed are actors a fresh positive fact proves enabled, non-service, and a member of a configured
	// sanctioned admin group — eligible for PROMOTION to attributed-authorized (REQ-2317).
	Confirmed []string
	// Disabled are actors a fresh positive fact proves locked, disabled, or expired — triggering DEMOTION to
	// attributed-suspicious when they are otherwise sanctioned (REQ-2318). An actor that is neither Confirmed
	// nor Disabled (unknown, ambiguous, or unresolved) appears in NEITHER list — the fail-open default.
	Disabled []string
	// Warnings are advisory notes carrying the provider and domain only, never a principal or its memberships.
	Warnings []string
}
