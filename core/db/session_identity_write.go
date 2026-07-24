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
}

// SessionIdentity reads back the decision-tracer provenance persisted on a session_triage row (migration
// 0027). Read-only, parameter-bound ($1), never string-built. ok=false when the ref has no triage row yet
// (a queued/suppressed session): the caller distinguishes "unknown" from an empty-but-present record.
func (s *TriageStore) SessionIdentity(ctx context.Context, externalRef string) (SessionIdentity, bool, error) {
	var id SessionIdentity
	err := s.p.QueryRow(ctx,
		`SELECT prompt_version, seed_hash, model_tier FROM session_triage WHERE external_ref = $1`, externalRef).
		Scan(&id.PromptVersion, &id.SeedHash, &id.ModelTier)
	switch {
	case err == nil:
		return id, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return SessionIdentity{}, false, nil
	default:
		return SessionIdentity{}, false, fmt.Errorf("db: session identity read %s: %w", externalRef, err)
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
	return SessionIdentity{PromptVersion: row.PromptVersion, SeedHash: row.SeedHash, ModelTier: row.ModelTier}, true, nil
}
