package suppression

import (
	"context"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/ingest"
)

// Stage is one deterministic suppression phase. A stage that matches returns a suppressing Decision; a
// stage that does not match (or hits an unconfirmed/expired registry row) returns an OutcomeEscalate
// pass-through so the chain continues (fail open). A stage error is caught by the chain and treated as a
// pass-through, never as a suppression.
type Stage interface {
	Name() Phase
	Evaluate(ctx context.Context, a Alert, now time.Time) (Decision, error)
}

// Chain is the deterministic pre-model suppression admission filter. It runs its stages
// most-specific-first and returns the FIRST non-escalate decision. Two properties hold by construction:
// a critical/unknown-severity alert always escalates before any registry is read (REQ-407); and every
// phase fails OPEN — a panic or error escalates rather than suppresses (REQ-405).
type Chain struct {
	Stages []Stage       // in order: dedup, blast-radius, scheduled, known-pattern, active-memory
	Freeze *FreezeGate   // declared maintenance/chaos windows, consulted BEFORE the severity floor
	Ledger *audit.Ledger // one immutable decision record per run (INV-19)
}

// Decide runs the chain and appends exactly one decision record to the governance ledger. It never
// returns an error for a phase failure — a phase failure is a fail-open pass-through.
func (c *Chain) Decide(ctx context.Context, a Alert, now time.Time) (Decision, error) {
	d := c.decide(ctx, a, now)
	if c.Ledger != nil {
		key := "suppress:" + a.ExternalRef
		if _, err := c.Ledger.Append(audit.GovDecision{
			Decision: "suppress:" + d.Outcome.String(),
			Reason:   string(d.Phase) + ":" + d.Reason,
			ActionID: key,
			Withheld: d.Outcome.Suppressing(), // suppressing withholds a remediation session
		}); err != nil {
			return d, err
		}
	}
	return d, nil
}

// decide is the pure decision: the severity floor, then each stage in order.
func (c *Chain) decide(ctx context.Context, a Alert, now time.Time) Decision {
	// A declared maintenance/chaos freeze is consulted BEFORE the severity floor: within a scoped, active
	// window the expected alert is suppressed even if critical, because the operator declared it. Narrow by
	// design — only in-scope alerts freeze; an unexpected/out-of-scope alert still hits the severity floor.
	if w, ok := c.Freeze.Frozen(a, now); ok {
		return Decision{Outcome: OutcomeSuppressed, Phase: PhaseSeverity, Reason: "declared maintenance/chaos freeze: " + w.Reason, ExternalRef: a.ExternalRef}
	}
	// Severity floor FIRST (REQ-407). A critical or unrecognized severity short-circuits every phase
	// before any registry is consulted; an unrecognized severity is never-suppress, never low-by-omission.
	if a.Severity == ingest.SeverityCritical || a.Severity == ingest.SeverityUnknown {
		return escalate(a.ExternalRef, PhaseSeverity, "critical-or-unknown severity is never suppressed")
	}
	for _, s := range c.Stages {
		d, err := c.safeEval(ctx, s, a, now)
		if err != nil {
			// a phase error fails OPEN — pass through to the next phase, never suppress (REQ-405).
			continue
		}
		if d.Outcome != OutcomeEscalate {
			return d // the first non-escalate decision wins
		}
	}
	return escalate(a.ExternalRef, PhaseEscalate, "no phase matched")
}

// safeEval runs a stage and recovers a panic into an error, so a panicking phase fails open.
func (c *Chain) safeEval(ctx context.Context, s Stage, a Alert, now time.Time) (d Decision, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("suppression: phase %s panicked: %v", s.Name(), r)
		}
	}()
	return s.Evaluate(ctx, a, now)
}
