package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/seal"
)

// SealedSecretStore is the pgx-backed sealed-secret store (task #27 Phase D, REQ-524). Rows hold
// ONLY envelope ciphertext (core/seal); the plaintext exists transiently in the grounder process at
// seal/open time and never here. Put is called ONLY from the worker's secret-put activity (after the
// ledger append); Get serves the store: SecretRef scheme; List feeds the value-less read surface.
type SealedSecretStore struct{ p *Pool }

// NewSealedSecretStore returns the Postgres-backed sealed-secret store.
func NewSealedSecretStore(p *Pool) *SealedSecretStore { return &SealedSecretStore{p: p} }

// SealedInfo is the value-LESS listing row (name + metadata; the type has no value field at all).
type SealedInfo struct {
	Name      string
	Purpose   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Put upserts a sealed blob under its name. A re-put is a rotation: created_at is preserved,
// updated_at moves.
func (s *SealedSecretStore) Put(ctx context.Context, name string, blob seal.Sealed, purpose, createdBy string, ledgerSeq int64, schemaVersion int) error {
	_, err := s.p.Pool.Exec(ctx, `
		INSERT INTO sealed_secret (name, ciphertext, nonce, wrapped_dek, dek_nonce, purpose, created_by, ledger_seq, schema_version, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
		ON CONFLICT (name) DO UPDATE SET
			ciphertext = EXCLUDED.ciphertext,
			nonce = EXCLUDED.nonce,
			wrapped_dek = EXCLUDED.wrapped_dek,
			dek_nonce = EXCLUDED.dek_nonce,
			purpose = EXCLUDED.purpose,
			created_by = EXCLUDED.created_by,
			ledger_seq = EXCLUDED.ledger_seq,
			schema_version = EXCLUDED.schema_version,
			updated_at = now()`,
		name, blob.Ciphertext, blob.Nonce, blob.WrappedDEK, blob.DEKNonce, purpose, createdBy, ledgerSeq, schemaVersion)
	if err != nil {
		return fmt.Errorf("db: sealed put: %w", err)
	}
	return nil
}

// Get loads the sealed blob for a name (found=false when absent).
func (s *SealedSecretStore) Get(ctx context.Context, name string) (seal.Sealed, bool, error) {
	var blob seal.Sealed
	err := s.p.Pool.QueryRow(ctx,
		`SELECT ciphertext, nonce, wrapped_dek, dek_nonce FROM sealed_secret WHERE name = $1`, name).
		Scan(&blob.Ciphertext, &blob.Nonce, &blob.WrappedDEK, &blob.DEKNonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return seal.Sealed{}, false, nil
	}
	if err != nil {
		return seal.Sealed{}, false, fmt.Errorf("db: sealed get: %w", err)
	}
	return blob, true, nil
}

// List returns the value-less inventory, name order.
func (s *SealedSecretStore) List(ctx context.Context) ([]SealedInfo, error) {
	rows, err := s.p.Pool.Query(ctx,
		`SELECT name, purpose, created_at, updated_at FROM sealed_secret ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("db: sealed list: %w", err)
	}
	defer rows.Close()
	var out []SealedInfo
	for rows.Next() {
		var r SealedInfo
		if err := rows.Scan(&r.Name, &r.Purpose, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("db: sealed list scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
