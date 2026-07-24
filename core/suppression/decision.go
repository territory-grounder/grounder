// Package suppression implements Territory Grounder's deterministic tier-1 suppression chain: a
// pre-model admission filter run in code BEFORE any model is spent. It runs dedup → blast-radius fold →
// scheduled-reboot phase SR → host-agnostic known-pattern → active-memory, and EVERY phase fails OPEN
// (escalate/investigate). A critical-severity or unknown-severity alert always escalates and is never
// suppressed. Suppression knowledge is assembled at runtime from a temporally-bounded, live-verified,
// org-global registry — never hardcoded into a prompt.
//
// Provenance: [F] spec/005 (BEH-5), the predecessor scripts/lib/tier1_suppression.py chain · [O] INV-04
// (envelope-boundary validation), INV-20 (temporally-bounded live-verified registry, fails open),
// INV-19 (decision ledger) · [R] paradigm-rule 1 (single-org; estate is a filter, not an isolation
// boundary, ADR-0010).
package suppression

import (
	"time"

	"github.com/territory-grounder/grounder/core/ingest"
)

// Phase names the pipeline phase a decision was reached in.
type Phase string

const (
	PhaseSeverity        Phase = "severity-floor"
	PhaseDedup           Phase = "dedup"
	PhaseBlastRadius     Phase = "blast-radius"
	PhaseScheduledReboot Phase = "scheduled-reboot"
	PhaseKnownPattern    Phase = "known-pattern"
	PhaseActiveMemory    Phase = "active-memory"
	PhaseEscalate        Phase = "escalate"
)

// Outcome is the suppression decision. The ZERO VALUE is OutcomeEscalate — fail OPEN by construction —
// so any panic, unmatched branch, or dropped-error path yields escalation (investigate) rather than an
// accidental silent suppression. This is the suppression lane's realization of the fail-closed
// philosophy: for this lane, "closed" IS escalation. [O] INV-20, REQ-405.
type Outcome int

const (
	OutcomeEscalate   Outcome = iota // zero value — fail open; investigate
	OutcomeSuppressed                // suppressed with no remediation session
	OutcomeNotice                    // posted as a notice, no remediation session (blast-radius fold)
)

func (o Outcome) String() string {
	switch o {
	case OutcomeSuppressed:
		return "suppressed"
	case OutcomeNotice:
		return "notice"
	default:
		return "escalate"
	}
}

// Suppressing reports whether an outcome withholds a remediation session (suppressed or notice).
func (o Outcome) Suppressing() bool { return o == OutcomeSuppressed || o == OutcomeNotice }

// Decision is the required-field output of a chain run. Producing it with a missing field is a Go type
// error, which is how the persistence contract (INV-19) is enforced at the type level.
type Decision struct {
	Outcome     Outcome
	Phase       Phase
	Reason      string
	ExternalRef string
	Signals     map[string]string
}

// escalate builds a fail-open escalation decision for the given phase.
func escalate(ref string, phase Phase, reason string) Decision {
	return Decision{Outcome: OutcomeEscalate, Phase: phase, Reason: reason, ExternalRef: ref}
}

// Alert is the typed suppression-chain input, built from a validated IncidentEnvelope (spec/006). Its
// Severity is the exhaustive ingest enum; ObservedAt is grammar-checked (INV-04).
type Alert struct {
	ExternalRef string
	Host        string
	AlertRule   string
	Site        string
	Severity    ingest.Severity
	IsReboot    bool
	BootReason  string // the recorded boot reason, for the two-phase verify (REQ-406)
	ObservedAt  time.Time
}
