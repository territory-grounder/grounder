package db

import (
	"context"
	"fmt"
	"time"
)

// NativeRuleRow is one operator-authored native resolver entry (credential_native_rule, migration 0088):
// a single packed ParseRules rule plus its write provenance. The entry carries SecretRef REFERENCES only
// (env:/file:/store:/bao:/…), never a secret value (INV-13) — the write lane (temporal/nativerule)
// validates that through credential.ParseRules/NewBundle before any row exists.
type NativeRuleRow struct {
	ID        int64
	Entry     string
	Rationale string
	CreatedBy string
	CreatedAt time.Time
}

// CredentialNativeRuleStore is the pgx-backed store for the operator-authored native credential mapping
// (TG-109, spec/016 REQ-1610). It is deliberately narrow: List for the sync source + the console read,
// Insert/Delete for the single-writer worker lane. Parameters are always bound ($1) — no string-built SQL.
// The table is mutable operator CONFIG, not the audit spine — every write is ledgered by the worker lane
// before it lands here.
type CredentialNativeRuleStore struct{ p *Pool }

// NewCredentialNativeRuleStore returns the Postgres-backed native-rule store.
func NewCredentialNativeRuleStore(p *Pool) *CredentialNativeRuleStore {
	return &CredentialNativeRuleStore{p: p}
}

// List returns every native rule ordered by id (insertion order — stable for the console and for the
// sync source's row-addressed error reporting).
func (s *CredentialNativeRuleStore) List(ctx context.Context) ([]NativeRuleRow, error) {
	rows, err := s.p.Pool.Query(ctx, `
		SELECT id, entry, rationale, created_by, created_at
		FROM credential_native_rule
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("db: credential_native_rule list: %w", err)
	}
	defer rows.Close()
	var out []NativeRuleRow
	for rows.Next() {
		var r NativeRuleRow
		if err := rows.Scan(&r.ID, &r.Entry, &r.Rationale, &r.CreatedBy, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: credential_native_rule scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: credential_native_rule rows: %w", err)
	}
	return out, nil
}

// Insert appends one validated rule row and returns its id. The caller (the worker lane) has already
// validated the entry through credential.ParseRules and appended the governance record — this is the
// persist half only.
func (s *CredentialNativeRuleStore) Insert(ctx context.Context, entry, rationale, createdBy string) (int64, error) {
	var id int64
	err := s.p.Pool.QueryRow(ctx, `
		INSERT INTO credential_native_rule (entry, rationale, created_by)
		VALUES ($1, $2, $3)
		RETURNING id`, entry, rationale, createdBy).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("db: credential_native_rule insert: %w", err)
	}
	return id, nil
}

// Delete removes one rule row by id. It reports (false, nil) when no such row exists — the caller maps
// that to its typed not-found refusal — and errors only on a genuine database failure.
func (s *CredentialNativeRuleStore) Delete(ctx context.Context, id int64) (bool, error) {
	tag, err := s.p.Pool.Exec(ctx, `DELETE FROM credential_native_rule WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("db: credential_native_rule delete (id %d): %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}
