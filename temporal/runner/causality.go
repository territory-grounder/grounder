package runner

// CAUSALITY (TG-662; owner prompt 2026-11-07 §2.D). A verified target state — the healed object positively in its
// goal state, the host quiet, the collateral radius quiet — is NOT the same claim as "TG caused it". An execution
// that precedes a recovery does not exclude a spontaneous recovery, another operator's action or the fault
// injector's own restore. So the terminus records a SEPARATE causality classification next to the credit
// disposition, computed from TG's own durable evidence (never the model's word), and the clean graduation credit
// and the clean precedent lesson require `tg-caused`. Every other value is recorded as what it is and never
// collapses into "verified" — the G4A pre-registration consumes this field.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/attribution"
)

// The closed causality vocabulary. Persisted on session_triage.causality and terminus_record.causality.
const (
	// CausalityTGCaused — TG's execution preceded the recovery AND no other restore actor is evidenced: the
	// injector neither restored nor re-injected on the host in the interval, no provider recovery preceded the
	// execution, and the actor-evidence readers attribute no other actor to the host in the interval.
	CausalityTGCaused = "tg-caused"
	// CausalityExternal — another actor is evidenced (an operator's action on the host in the interval) or the
	// provider recovery PRECEDED TG's execution (a spontaneous or third-party recovery TG then acted on top of).
	CausalityExternal = "external"
	// CausalityInjectorRestore — the fault injector's restore (its own undo, or its carve-out identity acting on
	// the host) lands in the interval: the synthetic fault was put back by the injector, not healed by TG.
	CausalityInjectorRestore = "injector-restore"
	// CausalityIndeterminate — a reader is missing or failed, an overlapping injection confounds the interval, or
	// the target state was never verified: TG cannot tell its own effect from another actor's. Never credited.
	CausalityIndeterminate = "indeterminate"
)

// CausalityQuery names the subject and the interval the evidence readers survey. IncidentAt is when the
// incident was received (a recovery between it and ExecutedAt means the fault was already clearing), ExecutedAt
// the heal's instant, ClearedAt when the terminus confirmed the clear.
type CausalityQuery struct {
	Host       string
	TargetHost string
	AlertRule  string
	OpClass    string
	IncidentAt time.Time
	ExecutedAt time.Time
	ClearedAt  time.Time
}

// CausalityEvidence is what the composition root's readers saw. Each `Known=false` is a reader that could not
// answer; the classifier treats any unknown reader as indeterminate unless another reader POSITIVELY names a
// different actor (positive evidence of another cause always wins over an unreadable reader).
type CausalityEvidence struct {
	InjectorKnown          bool
	InjectorRestores       int
	InjectorInjections     int
	RecoveryKnown          bool
	RecoveredBeforeExecute bool
	ActorsKnown            bool
	ActorCovered           bool
	ActorTaxonomy          string
	ActorCandidates        int
	ActorActions           int
	ActorUnattributed      int
	Notes                  []string
}

// classifyCausality is the deterministic rule. targetVerified is the credit predicate's target-state part
// (match ∧ confirmed clear ∧ collateral surveyed quiet); haveEvidence is false when no reader seam is wired or
// the seam itself failed. It returns the classification and a one-line evidence record for the row.
func classifyCausality(targetVerified, haveEvidence bool, ev CausalityEvidence) (causality, evidence string) {
	if !targetVerified {
		return CausalityIndeterminate, "target state not verified clean — nothing to attribute"
	}
	if !haveEvidence {
		why := "no causality evidence available — TG cannot tell its own effect from another actor's"
		if len(ev.Notes) > 0 {
			why += " (" + strings.Join(ev.Notes, "; ") + ")"
		}
		return CausalityIndeterminate, why
	}
	parts := make([]string, 0, 4)
	if ev.InjectorKnown {
		parts = append(parts, fmt.Sprintf("injector: %d restore(s), %d injection(s) in the interval", ev.InjectorRestores, ev.InjectorInjections))
	} else {
		parts = append(parts, "injector: unreadable")
	}
	if ev.RecoveryKnown {
		parts = append(parts, fmt.Sprintf("recovery before execute: %v", ev.RecoveredBeforeExecute))
	} else {
		parts = append(parts, "recovery transitions: unreadable")
	}
	switch {
	case !ev.ActorsKnown:
		parts = append(parts, fmt.Sprintf("actors: subject unreadable (%s seen on the readers that answered, %d record(s))", orNone(ev.ActorTaxonomy), ev.ActorActions))
	case !ev.ActorCovered:
		parts = append(parts, fmt.Sprintf("actors: no reader covers the subject's audit trail (%d record(s) elsewhere)", ev.ActorActions))
	default:
		parts = append(parts, fmt.Sprintf("actors: %s (%d action record(s), %d candidate taxonomy(ies), %d unattributed, subject covered)", ev.ActorTaxonomy, ev.ActorActions, ev.ActorCandidates, ev.ActorUnattributed))
	}
	if len(ev.Notes) > 0 {
		parts = append(parts, strings.Join(ev.Notes, "; "))
	}
	evidence = strings.Join(parts, "; ")
	// POSITIVE evidence of another cause — from any reader that answered — wins over an unreadable sibling: the
	// actor taxonomy is read even when ActorsKnown is false (a failed subject read blinds the QUIET reading, never
	// the positive one). An empty taxonomy matches nothing.
	switch {
	case ev.InjectorKnown && ev.InjectorRestores > 0:
		return CausalityInjectorRestore, evidence
	case ev.ActorTaxonomy == attribution.AuthorizedTest.String():
		return CausalityInjectorRestore, evidence
	case ev.RecoveryKnown && ev.RecoveredBeforeExecute:
		return CausalityExternal, evidence
	case ev.ActorTaxonomy == attribution.AttributedAuthorized.String() || ev.ActorTaxonomy == attribution.AttributedSuspicious.String():
		return CausalityExternal, evidence
	case !ev.InjectorKnown || !ev.RecoveryKnown || !ev.ActorsKnown:
		return CausalityIndeterminate, evidence
	case !ev.ActorCovered:
		// A reader that answered but does not COVER the subject's audit trail (no coverage marker, no record for
		// the subject) is blind, not quiet: another actor could have acted unseen. Blind never reads tg-caused.
		return CausalityIndeterminate, evidence + "; no actor-evidence reader covers the subject — blind, not quiet"
	case ev.ActorUnattributed > 0:
		// A reader returned records the core could not attribute to the queried host (stamped with another
		// identity — a VMID, an FQDN): an actor may be hiding in them. Never quiet.
		return CausalityIndeterminate, evidence + "; records the attribution core could not bind to the subject — not quiet"
	case ev.ActorCandidates > 1:
		return CausalityIndeterminate, evidence + "; more than one actor taxonomy is candidate — ambiguous"
	case ev.InjectorInjections > 0:
		return CausalityIndeterminate, evidence + "; an overlapping injection confounds the interval"
	}
	return CausalityTGCaused, evidence
}

func orNone(s string) string {
	if s == "" {
		return "nothing"
	}
	return s
}

// causalityEvidence runs the seam for one heal. haveEvidence=false when the seam is nil or failed.
func (a *Activities) causalityEvidence(ctx context.Context, q CausalityQuery) (CausalityEvidence, bool) {
	if a.D.CausalityEvidence == nil {
		return CausalityEvidence{}, false
	}
	ev, err := a.D.CausalityEvidence(ctx, q)
	if err != nil {
		return CausalityEvidence{Notes: []string{"causality evidence read failed: " + err.Error()}}, false
	}
	return ev, true
}
