package db

// terminus_record (TG-662, migration 0122; owner prompt 2026-11-07 §2.D): the APPEND-ONLY per-terminus record of
// what the credit predicate saw and decided — target state, collateral, the explicit credit disposition, whether
// the ladder was fed, and the causality classification with its evidence. One row per session terminus; never
// updated (tg_runtime holds SELECT, INSERT only). The G4A pre-registration and the credit audit (TG-666) read
// it; corrections are appended elsewhere, never written over these rows.

import (
	"context"
	"fmt"
	"time"
)

// TerminusRecord is one row. Zero ExecutedAt / ClearedAt persist as NULL.
type TerminusRecord struct {
	ExternalRef       string
	ActionID          string
	OpClass           string
	Host              string
	TargetHost        string
	Site              string
	ExecutedAt        time.Time
	ClearedAt         time.Time
	Executed          bool
	HasVerdict        bool
	Verdict           string
	ConfirmedClear    bool
	ClearReason       string
	ObjectState       string
	Collateral        string
	CreditDisposition string
	CreditFed         bool
	CreditClean       bool
	Causality         string
	CausalityEvidence string
	WindowState       string
	// Source is "terminus" (the session terminus; the default when empty) or "window" (the commit-confirm window's
	// resolution feed). A window row is appended at most once per (external_ref, action_id).
	Source     string
	RecordedAt time.Time
}

// Terminus record sources (the source column's closed vocabulary).
const (
	TerminusRecordSourceTerminus = "terminus"
	TerminusRecordSourceWindow   = "window"
)

// TerminusRecordStore appends and reads terminus_record rows.
type TerminusRecordStore struct{ p *Pool }

// NewTerminusRecordStore binds the store to a pool.
func NewTerminusRecordStore(p *Pool) *TerminusRecordStore { return &TerminusRecordStore{p: p} }

// Append inserts one record. It refuses an empty external_ref or op_class (a record nobody can join is noise).
func (s *TerminusRecordStore) Append(ctx context.Context, r TerminusRecord) error {
	if r.ExternalRef == "" || r.OpClass == "" {
		return fmt.Errorf("db: terminus record needs external_ref and op_class (got %q / %q)", r.ExternalRef, r.OpClass)
	}
	var executedAt, clearedAt any
	if !r.ExecutedAt.IsZero() {
		executedAt = r.ExecutedAt
	}
	if !r.ClearedAt.IsZero() {
		clearedAt = r.ClearedAt
	}
	source := r.Source
	if source == "" {
		source = TerminusRecordSourceTerminus
	}
	if _, err := s.p.Exec(ctx, `
		INSERT INTO terminus_record (external_ref, action_id, op_class, host, target_host, site, executed_at, cleared_at,
		                             executed, has_verdict, verdict, confirmed_clear, clear_reason, object_state, collateral,
		                             credit_disposition, credit_fed, credit_clean, causality, causality_evidence, window_state,
		                             source)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
		ON CONFLICT (external_ref, action_id) WHERE source = 'window' DO NOTHING`,
		r.ExternalRef, r.ActionID, r.OpClass, r.Host, r.TargetHost, r.Site, executedAt, clearedAt,
		r.Executed, r.HasVerdict, r.Verdict, r.ConfirmedClear, r.ClearReason, r.ObjectState, r.Collateral,
		r.CreditDisposition, r.CreditFed, r.CreditClean, r.Causality, r.CausalityEvidence, r.WindowState, source); err != nil {
		return fmt.Errorf("db: append terminus record %s: %w", r.ExternalRef, err)
	}
	return nil
}
