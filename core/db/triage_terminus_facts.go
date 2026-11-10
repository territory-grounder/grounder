package db

// The session TERMINUS FACTS (TG-662, migration 0122): the orchestrator-confirmed clear, the terminus
// collateral re-check's reading, and when they settled — written ONCE by the Runner workflow after its bounded
// clear loop and collateral re-check, read by the commit-confirm window's consult. They are captured DATA
// (INV-11): host-quiet readings from the alert surface, an object-state read from the diagnostic lane and the
// guest-liveness projection, and TG's own durable per-delivery alert capture — never the acting model's word.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TerminusFacts is one session's settled terminus. CollateralKnown=false means the re-check could NOT observe
// (stored NULL) — unknown is indeterminate, never quiet. FactsAt is nil until the terminus has settled; a window
// consulting a session with nil FactsAt reads PENDING, never clear.
type TerminusFacts struct {
	Mutated            bool
	ConfirmedClear     bool
	ConfirmedClearAt   *time.Time
	CollateralKnown    bool
	CollateralObserved bool
	FactsAt            *time.Time
	// Causality is the terminus classification (tg-caused | external | injector-restore | indeterminate; "" on a
	// legacy row) and CausalityEvidence its one-line record. Only tg-caused credits (owner prompt 2026-11-07 §2.D).
	Causality         string
	CausalityEvidence string
}

// MarkTerminusFacts persists the settled facts onto an already-recorded triage row. It supersedes MarkCleared
// for current-version workflows (the legacy activity keeps writing `confirmed_clear` alone for in-flight
// histories): one UPDATE carries the clear, its timestamp, the collateral reading (NULL when unknown) and the
// settle instant. A missing row updates zero rows (best-effort, like every triage follow-up). Keyed on
// external_ref, the triage PK.
func (s *TriageStore) MarkTerminusFacts(ctx context.Context, externalRef string, f TerminusFacts) error {
	if externalRef == "" {
		return fmt.Errorf("db: mark-terminus-facts with empty external_ref refused")
	}
	var collateral any
	if f.CollateralKnown {
		collateral = f.CollateralObserved
	}
	var causality, evidence any
	if f.Causality != "" {
		causality = f.Causality
		evidence = f.CausalityEvidence
	}
	if _, err := s.p.Exec(ctx, `
		UPDATE session_triage
		   SET confirmed_clear     = $2,
		       confirmed_clear_at  = CASE WHEN $3 THEN now() ELSE NULL END,
		       collateral_observed = $4,
		       causality           = $5,
		       causality_evidence  = $6,
		       terminus_facts_at   = now()
		 WHERE external_ref = $1`,
		externalRef, f.ConfirmedClear, f.ConfirmedClear, collateral, causality, evidence); err != nil {
		return fmt.Errorf("db: mark terminus facts %s: %w", externalRef, err)
	}
	return nil
}

// TerminusFactsFor reads one session's facts. found=false means no triage row exists for the ref (the consult
// treats that as pending, never as clear). A row whose terminus has not settled returns found=true with a nil
// FactsAt — the same pending reading.
func (s *TriageStore) TerminusFactsFor(ctx context.Context, externalRef string) (TerminusFacts, bool, error) {
	if externalRef == "" {
		return TerminusFacts{}, false, fmt.Errorf("db: terminus-facts read with empty external_ref refused")
	}
	var (
		f          TerminusFacts
		collateral *bool
	)
	err := s.p.QueryRow(ctx, `
		SELECT mutated, confirmed_clear, confirmed_clear_at, collateral_observed, terminus_facts_at,
		       COALESCE(causality, ''), COALESCE(causality_evidence, '')
		  FROM session_triage
		 WHERE external_ref = $1`, externalRef).
		Scan(&f.Mutated, &f.ConfirmedClear, &f.ConfirmedClearAt, &collateral, &f.FactsAt, &f.Causality, &f.CausalityEvidence)
	if errors.Is(err, pgx.ErrNoRows) {
		return TerminusFacts{}, false, nil
	}
	if err != nil {
		return TerminusFacts{}, false, fmt.Errorf("db: terminus facts for %s: %w", externalRef, err)
	}
	if collateral != nil {
		f.CollateralKnown = true
		f.CollateralObserved = *collateral
	}
	return f, true, nil
}
