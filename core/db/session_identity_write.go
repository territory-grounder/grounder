package db

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/judge"
)

// SessionIdentity is the prompt/seed/model PROVENANCE persisted on a session_triage row for the decision
// tracer (spec/020 T-020-9, REQ-2009): the trusted-preamble template version, the SHA-256 fingerprint of the
// composed agent seed (the HASH only — the seed embeds untrusted incident data, so never its text; INV-13),
// and the LLM tier the session ran on. NON-SECRET by construction — none of the three can carry argv, a host,
// or a credential. Observability only; nothing here re-enters the decision path.
type SessionIdentity struct {
	PromptVersion string
	SeedHash      string
	ModelTier     string
	// DecisionTier is the tier that produced the TERMINAL proposal/stop — which model DECIDED (TG-198,
	// migration 0057). ModelTier is the tier the read-only INVESTIGATION ran on; the TG-60 decide-nudge makes
	// them differ, and recording only the first attributed every decision in the corpus to the cheap tier.
	// Empty = the session predates the column (unattributable, NOT "fast").
	DecisionTier string
}

// SessionIdentity reads back the decision-tracer provenance persisted on a session_triage row (migration
// 0027). Read-only, parameter-bound ($1), never string-built. ok=false when the ref has no triage row yet
// (a queued/suppressed session): the caller distinguishes "unknown" from an empty-but-present record.
func (s *TriageStore) SessionIdentity(ctx context.Context, externalRef string) (SessionIdentity, bool, error) {
	var id SessionIdentity
	err := s.p.QueryRow(ctx,
		`SELECT prompt_version, seed_hash, model_tier, decision_model_tier FROM session_triage WHERE external_ref = $1`, externalRef).
		Scan(&id.PromptVersion, &id.SeedHash, &id.ModelTier, &id.DecisionTier)
	switch {
	case err == nil:
		return id, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return SessionIdentity{}, false, nil
	default:
		return SessionIdentity{}, false, fmt.Errorf("db: session identity read %s: %w", externalRef, err)
	}
}

// DegradedCapabilities reads back the self-dependency degraded set stamped on a session_triage row (TG-394
// slice 3, migration 0082) — the capabilities (embed / journal-evidence / secrets / tracker / notify) that
// were degraded when the session ran, so a lexical-only investigation is legible afterwards. Read-only,
// parameter-bound ($1). ok=false when the ref has no triage row. A NULL column (a row that predates the
// field) scans to a nil set with ok=true — the row EXISTS, which is distinct from "this build recorded
// nothing" (an explicit empty array). OBSERVABILITY ONLY — the set re-enters no gate.
func (s *TriageStore) DegradedCapabilities(ctx context.Context, externalRef string) ([]string, bool, error) {
	var caps []string
	err := s.p.QueryRow(ctx,
		`SELECT degraded_capabilities FROM session_triage WHERE external_ref = $1`, externalRef).Scan(&caps)
	switch {
	case err == nil:
		return caps, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("db: degraded capabilities read %s: %w", externalRef, err)
	}
}

// MemTriageStore is the in-memory twin of the session_triage writer for the CI oracles (no Postgres): it
// records each RecordTriage row keyed by external_ref (first-wins, mirroring the pgx ON CONFLICT DO NOTHING)
// and reads back its decision-tracer provenance. An acceptance oracle drives it to prove the write→read seam
// carries prompt_version/seed_hash/model_tier WITHOUT a database; the pgx TriageStore round-trip
// (session_identity_write_test.go, DSN-gated) proves the real SQL actually persists them (a fake alone can
// hide a dropped column — the reason both halves exist). Concurrency-safe.
type MemTriageStore struct {
	mu   sync.Mutex
	rows map[string]judge.TriageRow
}

// NewMemTriageStore returns an empty in-memory session_triage twin.
func NewMemTriageStore() *MemTriageStore { return &MemTriageStore{rows: map[string]judge.TriageRow{}} }

// RecordTriage records the terminal triage row — first-wins on external_ref (a duplicate is a no-op), exactly
// like the pgx writer's idempotent ON CONFLICT DO NOTHING.
func (m *MemTriageStore) RecordTriage(_ context.Context, row judge.TriageRow) error {
	if row.ExternalRef == "" {
		return errors.New("db: triage record with empty external_ref refused")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[row.ExternalRef]; !ok {
		m.rows[row.ExternalRef] = row
	}
	return nil
}

// SessionIdentity reads back the provenance recorded for a ref (ok=false when unrecorded).
func (m *MemTriageStore) SessionIdentity(_ context.Context, externalRef string) (SessionIdentity, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[externalRef]
	if !ok {
		return SessionIdentity{}, false, nil
	}
	return SessionIdentity{PromptVersion: row.PromptVersion, SeedHash: row.SeedHash, ModelTier: row.ModelTier,
		DecisionTier: row.DecisionTier}, true, nil
}

// DegradedCapabilities returns the self-dependency degraded set recorded for a ref (TG-394 slice 3), the
// in-memory twin of the pgx reader (ok=false when unrecorded). The pgx round-trip is what proves the SQL
// actually persists the column — a fake alone round-trips a column missing from the INSERT perfectly.
func (m *MemTriageStore) DegradedCapabilities(_ context.Context, externalRef string) ([]string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[externalRef]
	if !ok {
		return nil, false, nil
	}
	return row.DegradedCapabilities, true, nil
}
