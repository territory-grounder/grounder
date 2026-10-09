package db

import (
	"context"
	"fmt"
	"time"
)

// THE RETENTION BOUND FOR THE SCREENED TOOL-OUTPUT CORPUS (TG-295).
//
// agent_step_evidence (0053) stores verbatim host output — screened and scrubbed, but not sealed — and 0053
// gave it an append-only grant with no erasure path at all: not for the runtime, not for anyone. That made
// the corpus permanent rather than merely unrewritable. docs/DATA-MODEL.md §5.2 puts raw, high-cardinality
// content in the PURGEABLE operational body under a configurable TTL (INV-14); only the derived,
// de-identified audit spine is kept forever (§5.1). These constants and the call below are that TTL.
const (
	// DefaultEvidenceRetention is deliberately generous, because the audit value is real: the console's
	// "ground truth" citation is worthless if the payload behind a step it still lists has been reaped. A
	// year covers every retrospective anyone has actually asked for on this estate and still terminates.
	DefaultEvidenceRetention = 365 * 24 * time.Hour

	// EvidenceRetentionFloor mirrors the 24h floor enforced inside reap_agent_step_evidence. The database is
	// the real control (it holds when this code is bypassed or wrong); this constant exists so a misconfigured
	// TG_EVIDENCE_RETENTION is corrected once at boot instead of raising the same exception every sweep
	// forever — a reaper that errors on every tick is a retention bound that is not being enforced.
	EvidenceRetentionFloor = 24 * time.Hour

	// DefaultEvidenceReapBatch bounds one sweep. The first sweep after an operator shortens retention can be
	// arbitrarily large, and one unbounded DELETE holds locks over the table the agent is still writing to.
	// Bounded batches drain across ticks instead.
	DefaultEvidenceReapBatch = 50000
)

// ClampEvidenceRetention returns the retention the reaper will actually use, never shorter than the floor the
// database enforces. An operator who sets TG_EVIDENCE_RETENTION=1h is asking for something the SECURITY
// DEFINER path refuses by design; honouring it literally would mean every sweep raises, nothing is ever
// deleted, and the log fills with an error about a knob rather than the deployment losing its retention
// bound. Clamping keeps the bound enforced at the closest legal value and lets the caller say so once.
func ClampEvidenceRetention(d time.Duration) time.Duration {
	if d < EvidenceRetentionFloor {
		return EvidenceRetentionFloor
	}
	return d
}

// ReapEvidenceOlderThan deletes up to maxRows agent_step_evidence rows created strictly before cutoff and
// returns how many went.
//
// It calls reap_agent_step_evidence rather than issuing a DELETE, and there is no DELETE anywhere in this
// package for this table, because tg_runtime HAS no DELETE on it (0053) and must not be given one: a DELETE
// grant is a privilege over every row, including the one an attacker would most want gone, while the function
// can only ever remove rows older than a cutoff and writes agent_step_evidence_reap in the same transaction.
// See migration 0055 for the full justification.
//
// A cutoff inside the database's 24h floor comes back as an error from Postgres, deliberately — the floor is
// a control, not a hint, and swallowing it here would hide the one call that should never be made.
func (s *AgentStepEvidenceStore) ReapEvidenceOlderThan(ctx context.Context, cutoff time.Time, maxRows int) (int64, error) {
	if maxRows <= 0 {
		maxRows = DefaultEvidenceReapBatch
	}
	var deleted int64
	if err := s.p.QueryRow(ctx, `SELECT reap_agent_step_evidence($1, $2)`, cutoff, maxRows).Scan(&deleted); err != nil {
		return 0, fmt.Errorf("db: reap agent_step_evidence older than %s: %w", cutoff.UTC().Format(time.RFC3339), err)
	}
	return deleted, nil
}
