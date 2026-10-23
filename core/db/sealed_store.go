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

// bytesOrEmpty renders a nil slice as an EMPTY bytea rather than SQL NULL (TG-276).
//
// THE DEFECT: a DEK wrapper that carries its own nonce returns nil for the separate nonce — OpenBao
// Transit does exactly this, and says so at core/seal/transit.go:91 ("nonce is nil (Transit carries its
// own)"). pgx renders a nil []byte as NULL, and sealed_secret.dek_nonce is `bytea NOT NULL`. So EVERY
// Transit-backed write died on SQLSTATE 23502, and on a Transit deployment the sealed store could never
// hold a single row. Measured live: the first SecretPutWorkflow ever executed on this system failed here.
//
// NULL is relaxed rather than the constraint because the two mean different things. NULL is "unknown".
// Empty is "there is no separate nonce", which is the truth for a self-describing ciphertext — and the
// unwrap path already ignores this field for such wrappers. Keeping NOT NULL keeps the real guarantee:
// nobody may store a blob whose nonce was forgotten.
func bytesOrEmpty(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
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
		name, bytesOrEmpty(blob.Ciphertext), bytesOrEmpty(blob.Nonce), bytesOrEmpty(blob.WrappedDEK),
		bytesOrEmpty(blob.DEKNonce), purpose, createdBy, ledgerSeq, schemaVersion)
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

// WrappedDEKRow is one row's KEY-side material only: enough to rewrap the DEK, and nothing else. There is
// deliberately no ciphertext field — a rewrap never reads, moves, or re-encrypts the secret VALUE, so the
// value ciphertext must not even be loaded into the process that performs one (TG-163).
type WrappedDEKRow struct {
	Name       string
	WrappedDEK []byte
	DEKNonce   []byte
}

// ListWrappedDEKs returns every row's wrapped DEK in NAME order, so an interrupted rewrap run resumes at a
// deterministic point instead of re-walking an arbitrary order.
func (s *SealedSecretStore) ListWrappedDEKs(ctx context.Context, afterName string) ([]WrappedDEKRow, error) {
	rows, err := s.p.Pool.Query(ctx,
		`SELECT name, wrapped_dek, dek_nonce FROM sealed_secret WHERE name > $1 ORDER BY name`, afterName)
	if err != nil {
		return nil, fmt.Errorf("db: sealed list wrapped: %w", err)
	}
	defer rows.Close()
	var out []WrappedDEKRow
	for rows.Next() {
		var r WrappedDEKRow
		if err := rows.Scan(&r.Name, &r.WrappedDEK, &r.DEKNonce); err != nil {
			return nil, fmt.Errorf("db: sealed list wrapped scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RewrapDEK swaps ONE row's wrapped DEK for a re-wrapped one, and reports whether it landed (TG-163).
//
// THE RACE THIS REFUSES TO LOSE. A rewrap run reads a row, asks the key service to re-wrap that row's DEK,
// then writes the result back. In between, an administrator can re-put the same secret through
// PutSecretActivity — which replaces ciphertext AND wrapped_dek with a completely new DEK. An
// unconditional `UPDATE … SET wrapped_dek = $new WHERE name = $1` would then stamp the OLD secret's DEK
// over the NEW secret's ciphertext, and that row is destroyed: the value is gone, no plaintext exists
// anywhere to restore it from, and nothing surfaces until the next store: resolution fails. The window is
// small and the consequence is total, which is exactly the shape of bug that survives review and eats a
// credential a year later.
//
// So the WHERE clause pins the exact bytes the caller rewrapped from. Losing the race returns false, not
// an error: the row was re-put, which means it is already wrapped under the current key version, so the
// end state the operator asked for is the end state they have.
//
// updated_at is deliberately NOT moved. A rewrap does not change the secret, and the console's inventory
// column says when the credential last changed; bumping it would report a rotation of every credential in
// the estate to anyone reading that page.
func (s *SealedSecretStore) RewrapDEK(ctx context.Context, name string, oldWrapped, oldNonce, newWrapped, newNonce []byte) (bool, error) {
	tag, err := s.p.Pool.Exec(ctx, `
		UPDATE sealed_secret SET wrapped_dek = $4, dek_nonce = $5
		 WHERE name = $1 AND wrapped_dek = $2 AND dek_nonce = $3`,
		name, bytesOrEmpty(oldWrapped), bytesOrEmpty(oldNonce),
		bytesOrEmpty(newWrapped), bytesOrEmpty(newNonce))
	if err != nil {
		return false, fmt.Errorf("db: sealed rewrap: %w", err)
	}
	return tag.RowsAffected() == 1, nil
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
