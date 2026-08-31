package db

import (
	"context"
	"fmt"
	"time"
)

// CredentialBindingRow is one published (source, credential, scope) binding — the credential-onboarding
// work list (TG-274). References only; there is no column a secret value could occupy (INV-13).
type CredentialBindingRow struct {
	SourceID   string
	Credential string
	Scope      string
	Via        string
	Hosts      int
	Mapped     bool
	SecretRef  string
	ObservedAt time.Time
}

// CredentialBindingProjectionStore is the pgx writer/reader for credential_binding_projection (0054).
type CredentialBindingProjectionStore struct{ p *Pool }

func NewCredentialBindingProjectionStore(p *Pool) *CredentialBindingProjectionStore {
	return &CredentialBindingProjectionStore{p: p}
}

// Publish replaces this source's bindings with what the latest sync observed.
//
// DELETE-THEN-INSERT, in one transaction, scoped to the SOURCE. An upsert alone would leave a credential
// the operator deleted in AWX on the screen forever, and a surface that shows things which no longer exist
// stops being read. Scoped to the source so one connector's sync never erases another's.
func (s *CredentialBindingProjectionStore) Publish(ctx context.Context, sourceID string, rows []CredentialBindingRow, now time.Time) error {
	if sourceID == "" {
		return fmt.Errorf("db: credential binding publish with empty source id refused")
	}
	tx, err := s.p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: credential binding publish begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM credential_binding_projection WHERE source_id = $1`, sourceID); err != nil {
		return fmt.Errorf("db: credential binding clear %s: %w", sourceID, err)
	}
	for _, r := range rows {
		if _, err := tx.Exec(ctx, `
			INSERT INTO credential_binding_projection
				(source_id, credential, scope, via, hosts, mapped, secret_ref, observed_at, schema_version)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,1)
			ON CONFLICT (source_id, credential, scope) DO UPDATE SET
				via = EXCLUDED.via, hosts = EXCLUDED.hosts, mapped = EXCLUDED.mapped,
				secret_ref = EXCLUDED.secret_ref, observed_at = EXCLUDED.observed_at`,
			sourceID, r.Credential, r.Scope, r.Via, r.Hosts, r.Mapped, r.SecretRef, now); err != nil {
			return fmt.Errorf("db: credential binding insert %s/%s: %w", sourceID, r.Credential, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: credential binding publish commit: %w", err)
	}
	return nil
}

// Bindings reads every published binding, UNMAPPED FIRST then widest blast radius — the rows an operator
// must act on lead, so a long tail of working credentials cannot push them off the screen.
func (s *CredentialBindingProjectionStore) Bindings(ctx context.Context) ([]CredentialBindingRow, error) {
	rows, err := s.p.Query(ctx, `
		SELECT source_id, credential, scope, via, hosts, mapped, secret_ref, observed_at
		FROM credential_binding_projection
		ORDER BY mapped ASC, hosts DESC, credential ASC, scope ASC`)
	if err != nil {
		return nil, fmt.Errorf("db: credential binding read: %w", err)
	}
	defer rows.Close()
	var out []CredentialBindingRow
	for rows.Next() {
		var r CredentialBindingRow
		if err := rows.Scan(&r.SourceID, &r.Credential, &r.Scope, &r.Via, &r.Hosts, &r.Mapped, &r.SecretRef, &r.ObservedAt); err != nil {
			return nil, fmt.Errorf("db: scan credential binding: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
