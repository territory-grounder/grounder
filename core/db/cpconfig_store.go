package db

import (
	"context"
	"fmt"

	"github.com/territory-grounder/grounder/core/cpconfig"
)

// CPConfigStore is the pgx-backed console-override store (task #27 Phase C, REQ-523): the durable
// twin of the resolver's ConsoleStore seam. Reads feed cpconfig.Resolver (which STILL clamps — an
// override row for a LAW or non-writable key is structurally ignored at resolve time); the single
// writer is the worker's config-write workflow, never the grounder. Parameters are always bound —
// no string-built SQL.
type CPConfigStore struct{ p *Pool }

// NewCPConfigStore returns the Postgres-backed control-plane config override store.
func NewCPConfigStore(p *Pool) *CPConfigStore { return &CPConfigStore{p: p} }

// Overrides returns every stored override as key→value (the resolver filters legality per key).
func (s *CPConfigStore) Overrides(ctx context.Context) (map[string]string, error) {
	rows, err := s.p.Pool.Query(ctx, `SELECT key, value FROM control_plane_config`)
	if err != nil {
		return nil, fmt.Errorf("db: config overrides: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("db: config overrides scan: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// Upsert records the CURRENT override for a key (history lives on the governance ledger, whose seq is
// carried on the row). Called ONLY from the worker's config-write activity, after the ledger append.
func (s *CPConfigStore) Upsert(ctx context.Context, key, value, rationale, updatedBy string, ledgerSeq int64, schemaVersion int) error {
	_, err := s.p.Pool.Exec(ctx, `
		INSERT INTO control_plane_config (key, value, rationale, updated_by, ledger_seq, schema_version, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (key) DO UPDATE SET
			value = EXCLUDED.value,
			rationale = EXCLUDED.rationale,
			updated_by = EXCLUDED.updated_by,
			ledger_seq = EXCLUDED.ledger_seq,
			schema_version = EXCLUDED.schema_version,
			updated_at = now()`,
		key, value, rationale, updatedBy, ledgerSeq, schemaVersion)
	if err != nil {
		return fmt.Errorf("db: config upsert: %w", err)
	}
	return nil
}

// compile-time proof the durable store satisfies the resolver seam its in-memory twin also satisfies.
var _ cpconfig.ConsoleStore = (*CPConfigStore)(nil)

// Delete removes the stored override for key, returning whether a row was actually removed (TG-261).
//
// The console had no way to take an override back: the route was POST-only and ValidateWrite refuses an
// empty value, so a saved setting could be corrected but never REMOVED — and after TG-260 made the worker
// READ these rows, a value that stops the worker booting could only be undone by hand surgery on Postgres,
// because the only writer that can touch this table runs inside that worker.
//
// Deleting is the right verb rather than writing a sentinel: absence is exactly what "no override" means,
// and the resolver already treats an absent row as fall-through to env. A tombstone would need every
// reader to learn a second spelling of nothing.
func (s *CPConfigStore) Delete(ctx context.Context, key string) (bool, error) {
	tag, err := s.p.Exec(ctx, `DELETE FROM control_plane_config WHERE key = $1`, key)
	if err != nil {
		return false, fmt.Errorf("db: clear config override %s: %w", key, err)
	}
	return tag.RowsAffected() > 0, nil
}
