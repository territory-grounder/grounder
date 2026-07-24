// Package policy is Territory Grounder's operator-managed Policy Engine — the AUTHORIZATION half of
// governed actuation and the sibling of the Credential Engine (spec/016, which is AUTHENTICATION). The
// policy engine answers "MAY TG act on this target?"; the credential engine answers "with what identity?".
// They compose at the actuation chokepoint:
//
//	agent proposes {op-class, target} → POLICY: may this? (authZ) → CREDENTIAL: resolve identity (authN) → actuate
//
// This file set implements the FOUNDATION slice — spec/015 tasks T-015-1 (the embedded, fixed OPA/Rego
// evaluator with a deny-overrides effect) and T-015-2 (this file: the rule DATA model, the verdict trinary,
// and param inheritance). The four-mode state machine, band composition, min_confidence/rate clamps,
// graduation, identity/approve_by enforcement, persistence, and the MutationGate absorption are LATER
// leaves (T-015-3..13) and are intentionally NOT built here; this slice only CARRIES the struct fields
// those leaves will enforce (BandMode, MinConfidence, ApproveBy) without enforcing them.
//
// Rules are DATA, never Rego (REQ-1503): an operator supplies an ordered rule list; the FIXED audited Rego
// module in rego/policy.rego evaluates it. The load-bearing safety property is FAIL-CLOSED BY CONSTRUCTION
// (INV-09): a malformed rule is a load-time error (schema.go), an unknown verdict is a closed-enum error,
// and when no rule matches the evaluator resolves to `approve` (route to a human), never a silent `auto`.
//
// The rule's estate-object-model selector (host / host-glob / group / device-class / resource) is the ONE
// SHARED grammar built in core/credential (Selector/Target/Match, REQ-1605) — this package imports it
// rather than defining a second, divergent inventory matcher. Only the policy-specific match dimensions
// (op-class, argv-pattern, territory, reversible) live here.
//
// Provenance: [F] consolidates the scattered actuation gates into one access-control layer · [R]
// paradigm-rule 2 (three verdicts) · [O] INV-06/INV-07/INV-09/INV-11. See spec/015-policy-engine.
package policy

import (
	"strings"

	"github.com/territory-grounder/grounder/core/credential"
)

// Verdict is the trinary a policy evaluation resolves an action to (REQ-1506). It is a CLOSED enum: any
// other string is a load-time error (schema.go), so an operator cannot smuggle a fourth, unhandled outcome.
type Verdict string

const (
	// VerdictAuto authorizes execution under verify-on-auto (REQ-1515, enforced in a later leaf).
	VerdictAuto Verdict = "auto"
	// VerdictApprove routes the action to the rule's approve_by principals for a human vote (REQ-1516).
	VerdictApprove Verdict = "approve"
	// VerdictDeny refuses execution and records the refusal (deny-overrides, REQ-1504).
	VerdictDeny Verdict = "deny"
)

// validVerdict reports whether v is one of the three closed-enum verdicts.
func validVerdict(v Verdict) bool {
	switch v {
	case VerdictAuto, VerdictApprove, VerdictDeny:
		return true
	default:
		return false
	}
}

// BandMode selects how a rule's verdict composes with the action's spec/001 risk band (REQ-1509/1510).
// It is CARRIED here but ENFORCED in T-015-6; this slice only validates the closed enum.
type BandMode string

const (
	// BandRespect (the default) emits the more-restrictive of {policy verdict, risk band} (REQ-1509).
	BandRespect BandMode = "respect"
	// BandForce applies the policy verdict even when less restrictive, double-warned (REQ-1510). The
	// constitutional mechanical floor (INV-09) remains in force beneath a force override.
	BandForce BandMode = "force"
)

// validBandMode reports whether b is unset ("" — inherit) or a closed-enum band mode.
func validBandMode(b BandMode) bool {
	switch b {
	case "", BandRespect, BandForce:
		return true
	default:
		return false
	}
}

// DefaultMinConfidence is the hard floor a rule's min_confidence falls back to when neither the rule nor the
// global-default rule sets one (REQ-1507). An action whose bound confidence is below the resolved
// min_confidence is clamped to at most `approve` in T-015-3 (not enforced in this slice).
const DefaultMinConfidence = 0.60

// Params is a rule's tunable parameter set. Each field is a POINTER (or "" for BandMode) so that "unset"
// is distinguishable from "set to the zero value" — an unset field inherits from the global-default rule
// (REQ-1507). MinConfidence and RateLimit are carried here and enforced in T-015-3; BandMode in T-015-6.
type Params struct {
	MinConfidence *float64 // nil = inherit; resolved value clamps confidence in T-015-3 (REQ-1507).
	BandMode      BandMode // "" = inherit; composition mode in T-015-6 (REQ-1509/1510).
	RateLimit     *int     // nil = inherit; executions-per-minute governor in T-015-3 (REQ-1508).
}

// inherit fills each unset field of p from def (the global-default rule's params, REQ-1507) and returns the
// merged copy. A field already set on p is never overwritten.
func (p Params) inherit(def Params) Params {
	out := p
	if out.MinConfidence == nil {
		out.MinConfidence = def.MinConfidence
	}
	if out.BandMode == "" {
		out.BandMode = def.BandMode
	}
	if out.RateLimit == nil {
		out.RateLimit = def.RateLimit
	}
	return out
}

// Match is one rule's selector. Its estate-object-model dimension is the SHARED credential.Selector
// (host / host-glob / group / device-class / resource, REQ-1605) — deliberately NOT a second grammar. The
// remaining dimensions are policy-specific: OpClass (semantic, the allow side), ArgvPattern (raw command
// string, the deny side), Territory, and Reversible (REQ-1505). A rule matches an action WHEN every
// specified dimension matches (AND semantics) and at least one dimension is specified — an all-empty Match
// matches nothing (fail closed).
type Match struct {
	Selector    *credential.Selector // nil = no estate constraint; else the shared object-model matcher.
	OpClass     string               // semantic op-class, case-insensitive exact (the allow side).
	ArgvPattern string               // raw command-string pattern (the deny side); see argvMatch.
	Territory   string               // estate territory, case-insensitive exact.
	Reversible  *bool                // nil = don't-care; else must equal the action's reversibility.
}

// matches reports whether m matches the action described by in. Every specified dimension must match; an
// unspecified dimension is ignored; an all-unspecified Match matches nothing (REQ-1505 fail-closed).
func (m Match) matches(in EvalInput) bool {
	specified := false

	if m.Selector != nil {
		specified = true
		// The ONE shared estate object-model matcher (REQ-1605) — no second selector grammar.
		t := credential.Target{Host: in.Host, Resource: in.Resource, Groups: in.Groups, DeviceClass: in.DeviceClass}
		if !credential.Match(*m.Selector, t) {
			return false
		}
	}
	if m.OpClass != "" {
		specified = true
		if !strings.EqualFold(strings.TrimSpace(m.OpClass), strings.TrimSpace(in.OpClass)) {
			return false
		}
	}
	if m.ArgvPattern != "" {
		specified = true
		if !argvMatch(m.ArgvPattern, in.Argv) {
			return false
		}
	}
	if m.Territory != "" {
		specified = true
		if !strings.EqualFold(strings.TrimSpace(m.Territory), strings.TrimSpace(in.Territory)) {
			return false
		}
	}
	if m.Reversible != nil {
		specified = true
		if *m.Reversible != in.Reversible {
			return false
		}
	}
	return specified
}

// argvMatch matches a raw command string against a rule's argv deny-pattern (the deny side of REQ-1505).
// A pattern containing '*' uses the SAME leading/trailing glob semantics as the shared object-model
// (credential.GlobMatch); a pattern with no '*' is a case-insensitive substring test, the natural shape for
// a predecessor safe-exec deny fragment such as "rm -rf" or "mkfs". This is a command-string matcher, NOT
// an estate-object-model dimension, so it is policy-local and does not fork the shared selector grammar.
func argvMatch(pattern, argv string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if strings.Contains(pattern, "*") {
		return credential.GlobMatch(pattern, argv)
	}
	return strings.Contains(strings.ToLower(argv), strings.ToLower(pattern))
}

// Rule is one operator-declared policy entry — DATA, never Rego (REQ-1503/1505). ID is a stable, non-secret
// label carried into the decision projection and the audit ledger. IsDefault marks the single global-default
// rule whose Params seed inheritance (REQ-1507); a default rule needs no Match (it contributes params, not a
// verdict match) but still carries a valid verdict for schema completeness.
type Rule struct {
	ID        string
	Match     Match
	Verdict   Verdict
	Params    Params
	ApproveBy []string // user: / group: principals for an `approve` verdict (carried; enforced in T-015-9).
	IsDefault bool
}
