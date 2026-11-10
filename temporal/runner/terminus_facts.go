package runner

// TG-662 (G4A findings F3/F8): the session TERMINUS FACTS — the orchestrator-observed clear (host quiet AND the
// healed OBJECT positively in its goal state), the collateral re-check's reading, and when they settled —
// persisted on the triage row so the two consumers that decide autonomy read ONE record: the ladder credit
// (recordGraduationCredit, reconcile.go) and the commit-confirm window's consult (ConsultCommitConfirmActivity).
//
// Before this file the window confirmed on the frozen execute-time `match` alone, which EXCLUDES the target
// host (core/verify/verdict.go), so a heal whose target kept alerting was confirmed, credited and graduated; and
// ConfirmedClear itself was host-quiet only — a quiet host with the unit still down read as clear.

import (
	"context"
	"log"
	"time"
)

// ObjectStateReading is one OBJECT-specific read of the healed thing: the unit's active state, the container's
// state, the guest's power state. Observed=false is "could not read" — an unmonitored target, a refused or
// failed read, an unknown object kind — and is NEVER clear: the caller treats it as indeterminate. Healthy is
// meaningful only when Observed is true.
type ObjectStateReading struct {
	Healthy  bool
	Observed bool
	Detail   string
}

// TerminusFacts is the runner-side mirror of the persisted facts (db.TerminusFacts; the composition root maps,
// so the runner keeps its core/db import surface to the commit-confirm store). CollateralKnown=false means the
// collateral re-check could not observe (no graph members, no reader) — unknown is INDETERMINATE, never quiet.
// FactsAt is nil until the terminus has settled them.
type TerminusFacts struct {
	Mutated            bool
	ConfirmedClear     bool
	ConfirmedClearAt   *time.Time
	CollateralKnown    bool
	CollateralObserved bool
	FactsAt            *time.Time
	// Causality / CausalityEvidence: the terminus classification (causality.go) and its one-line evidence. "" on
	// a legacy row reads as indeterminate — never credited.
	Causality         string
	CausalityEvidence string
}

// ObjectStateInput names the healed object: the action's TARGET host, its op-class, and the sealed params
// (unit / container / guest — the composition root's reader picks the param by the class's family).
type ObjectStateInput struct {
	Host    string
	OpClass string
	Params  map[string]string
}

// ObjectStateResult is the activity's recorded reading (replay-safe: the read happens in the activity).
type ObjectStateResult struct {
	Healthy  bool
	Observed bool
	Detail   string
}

// ObserveObjectStateActivity reads the healed object's state through the composition root's reader. It NEVER
// returns an error for an unreadable object — unreadable is a RESULT (Observed=false) the workflow treats as
// indeterminate, exactly like ObserveClearedActivity's fail-closed "not cleared". A nil seam is unobserved.
func (a *Activities) ObserveObjectStateActivity(ctx context.Context, in ObjectStateInput) (ObjectStateResult, error) {
	if a.D.ObjectState == nil {
		return ObjectStateResult{Detail: "no object-state reader wired — the healed object cannot be confirmed"}, nil
	}
	r := a.D.ObjectState(ctx, in.Host, in.OpClass, in.Params)
	if !r.Observed {
		r.Healthy = false
	}
	return ObjectStateResult{Healthy: r.Healthy, Observed: r.Observed, Detail: r.Detail}, nil
}

// MarkTerminusFactsInput carries the settled facts for one session. CollateralKnown=false ⇒ the persisted
// collateral_observed is NULL (unknown), never false.
type MarkTerminusFactsInput struct {
	ExternalRef        string
	Mutated            bool
	ConfirmedClear     bool
	CollateralKnown    bool
	CollateralObserved bool
	// The causality interval and subject (owner prompt 2026-11-07 §2.D): the incident subject and the action's
	// target, the rule, when the incident was received, when the heal executed, when the clear confirmed, and
	// whether the fresh execute-time verdict read match. VerdictMatch ∧ ConfirmedClear ∧ collateral quiet is the
	// target-state half of the credit predicate; causality is classified only for a verified target state.
	Host         string
	TargetHost   string
	AlertRule    string
	OpClass      string
	IncidentAt   time.Time
	ExecutedAt   time.Time
	ClearedAt    time.Time
	VerdictMatch bool
}

// TerminusFactsResult is the recorded outcome: whether the facts persisted, and the causality the activity
// classified (replay-safe — the readers run here; the workflow forwards the recorded value to the terminus).
type TerminusFactsResult struct {
	Recorded          bool
	Causality         string
	CausalityEvidence string
}

// MarkTerminusFactsActivity classifies the heal's CAUSALITY from TG's own evidence readers and persists it with
// the facts onto the triage row (migration 0122). FAIL-OPEN like MarkTriageClearedActivity: a nil seam or a write
// error is logged, never an activity error. The consequence is LOAD-BEARING for the commit-confirm window — it
// reads these facts and never confirms without them, and never credits without tg-caused — so a lost write costs
// a credit and never grants one (the correct direction for an earn path). Falls back to the legacy
// TriageMarkCleared seam when no facts seam is wired (a composition root without the 0122 store).
func (a *Activities) MarkTerminusFactsActivity(ctx context.Context, in MarkTerminusFactsInput) (TerminusFactsResult, error) {
	targetVerified := in.Mutated && in.VerdictMatch && in.ConfirmedClear && in.CollateralKnown && !in.CollateralObserved
	var ev CausalityEvidence
	have := false
	if targetVerified {
		ev, have = a.causalityEvidence(ctx, CausalityQuery{
			Host: in.Host, TargetHost: in.TargetHost, AlertRule: in.AlertRule, OpClass: in.OpClass,
			IncidentAt: in.IncidentAt, ExecutedAt: in.ExecutedAt, ClearedAt: in.ClearedAt,
		})
	}
	causality, evidence := classifyCausality(targetVerified, have, ev)
	out := TerminusFactsResult{Causality: causality, CausalityEvidence: evidence}
	switch {
	case a.D.TriageMarkFacts != nil:
		if err := a.D.TriageMarkFacts(ctx, in.ExternalRef, TerminusFacts{
			Mutated: in.Mutated, ConfirmedClear: in.ConfirmedClear,
			CollateralKnown: in.CollateralKnown, CollateralObserved: in.CollateralObserved,
			Causality: causality, CausalityEvidence: evidence,
		}); err != nil {
			log.Printf("triage mark-terminus-facts %s: %v (best-effort — the session is unaffected; the window will not confirm without the facts)", in.ExternalRef, err)
		} else {
			out.Recorded = true
		}
	case a.D.TriageMarkCleared != nil:
		if err := a.D.TriageMarkCleared(ctx, in.ExternalRef, in.ConfirmedClear); err != nil {
			log.Printf("triage mark-cleared %s: %v (best-effort — the session is unaffected)", in.ExternalRef, err)
		} else {
			out.Recorded = true
		}
	}
	if in.Mutated && a.D.TriageMarkMutated != nil {
		if err := a.D.TriageMarkMutated(ctx, in.ExternalRef); err != nil {
			log.Printf("triage mark-mutated %s: %v (best-effort — the session is unaffected)", in.ExternalRef, err)
		}
	}
	return out, nil
}

// terminusFacts reads the persisted facts for one session through the seam. found=false (or a nil seam) means
// the facts are not readable — the caller treats that as PENDING, never as clear.
func (a *Activities) terminusFacts(ctx context.Context, externalRef string) (TerminusFacts, bool, error) {
	if a.D.TriageFacts == nil {
		return TerminusFacts{}, false, nil
	}
	return a.D.TriageFacts(ctx, externalRef)
}
