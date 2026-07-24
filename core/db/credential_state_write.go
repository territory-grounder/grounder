package db

import (
	"context"
	"fmt"
	"sync"

	"github.com/territory-grounder/grounder/core/credential"
)

// CredentialCoverage is one source's current, non-secret coverage summary: how many targets (host bundles or
// approver identities) the source presently covers, plus its identity plane. It carries NO secret material —
// only counts and the non-secret plane label — so it is safe to persist and surface in the console.
type CredentialCoverage struct {
	SourceID string
	Plane    credential.Plane
	Targets  int
}

// CredentialStateStore is the WRITE side of the credential-engine's coverage + sync-state projection
// (spec/016 credential engine, wired live in the worker): after each SyncAll the worker publishes, per
// source, the SyncRun record (its drift + last-synced + outcome) and the current coverage count. It is an
// interface so the worker composes it, the pgx impl runs against Postgres, and the in-memory fake drives the
// CI oracles (CI has no Postgres). Publish NEVER receives or writes a secret value — the SyncRun and
// CredentialCoverage types are secret-free by construction (INV-13).
type CredentialStateStore interface {
	Publish(ctx context.Context, runs []credential.SyncRun, coverage []CredentialCoverage) error
}

// CredentialStateWriteStore is the pgx-backed CredentialStateStore. It appends one credential_sync_run row
// per SyncRun and upserts one latest-wins credential_coverage row per source (migration 0017). Parameters
// are always bound ($1) — no string-built SQL. Only NON-SECRET fields are written.
type CredentialStateWriteStore struct{ p *Pool }

// NewCredentialStateWriteStore returns the Postgres-backed credential-state projection writer.
func NewCredentialStateWriteStore(p *Pool) *CredentialStateWriteStore { return &CredentialStateWriteStore{p: p} }

// Publish appends each SyncRun as an immutable credential_sync_run row and upserts each source's coverage
// summary. It writes ONLY the non-secret SyncRun/coverage fields (source id, plane, drift counts, outcome,
// non-secret error text, target count) — never a secret value, which these types cannot carry. A partial
// failure returns an error; the worker treats publication as best-effort (a write error is logged, never
// fatal), exactly like the estate snapshot publish.
func (s *CredentialStateWriteStore) Publish(ctx context.Context, runs []credential.SyncRun, coverage []CredentialCoverage) error {
	for _, r := range runs {
		var lastSynced any
		if !r.LastSyncedAt.IsZero() {
			lastSynced = r.LastSyncedAt.UTC()
		}
		_, err := s.p.Pool.Exec(ctx, `
			INSERT INTO credential_sync_run
				(source_id, plane, started_at, last_synced_at, added, changed, removed, outcome, err)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			r.SourceID, string(r.Plane), r.StartedAt.UTC(), lastSynced,
			r.Added, r.Changed, r.Removed, string(r.Outcome), r.Err)
		if err != nil {
			return fmt.Errorf("db: credential_sync_run insert (source %q): %w", r.SourceID, err)
		}
	}
	for _, c := range coverage {
		_, err := s.p.Pool.Exec(ctx, `
			INSERT INTO credential_coverage (source_id, plane, targets, updated_at)
			VALUES ($1, $2, $3, now())
			ON CONFLICT (source_id) DO UPDATE SET
				plane      = EXCLUDED.plane,
				targets    = EXCLUDED.targets,
				updated_at = now()`,
			c.SourceID, string(c.Plane), c.Targets)
		if err != nil {
			return fmt.Errorf("db: credential_coverage upsert (source %q): %w", c.SourceID, err)
		}
	}
	return nil
}

// MemCredentialStateStore is the in-memory CredentialStateStore twin for the CI oracles (no Postgres). It
// records every published run and the latest coverage per source so a test can assert exactly which fields
// were written (and that no secret ever leaks into a row). Concurrency-safe.
type MemCredentialStateStore struct {
	mu       sync.Mutex
	Runs     []credential.SyncRun
	Coverage map[string]CredentialCoverage
}

// NewMemCredentialStateStore returns an empty in-memory credential-state twin.
func NewMemCredentialStateStore() *MemCredentialStateStore {
	return &MemCredentialStateStore{Coverage: map[string]CredentialCoverage{}}
}

// Publish records the runs (append) and the latest coverage per source (upsert).
func (m *MemCredentialStateStore) Publish(_ context.Context, runs []credential.SyncRun, coverage []CredentialCoverage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Runs = append(m.Runs, runs...)
	for _, c := range coverage {
		m.Coverage[c.SourceID] = c
	}
	return nil
}

// compile-time proof both implementations satisfy the store interface.
var (
	_ CredentialStateStore = (*CredentialStateWriteStore)(nil)
	_ CredentialStateStore = (*MemCredentialStateStore)(nil)
)
