package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/territory-grounder/grounder/core/audit"
)

// LedgerStore is the pgx-backed, append-only writer for governance_ledger (migration 0003). It implements
// audit.LedgerSink, so an in-memory audit.Ledger mirrors every appended entry to Postgres write-through. The
// hash chain is computed by the Ledger (the DB stores the already-linked rows); a post-hoc DB edit is caught
// by audit.VerifyChain over All().
type LedgerStore struct{ p *Pool }

// NewLedgerStore returns a Postgres-backed governance-ledger writer.
func NewLedgerStore(p *Pool) *LedgerStore { return &LedgerStore{p: p} }

var (
	_ audit.LedgerSink        = (*LedgerStore)(nil)
	_ audit.LedgerSinkContext = (*LedgerStore)(nil)
)

// PersistContext appends one hash-chained governance entry under the CALLER's deadline. seq is the chain
// position supplied by the Ledger (the governance_ledger PK), so a duplicate seq is a hard error — never a
// silent overwrite of an audit row.
//
// THE DEFECT (TG-277): this INSERT used to run on context.Background(), so it was uncancellable. The
// Ledger holds its chain gate across this call, which made a stalled Postgres block every governance
// decision in the worker without limit, and made the calling activity spend its whole 15s
// StartToCloseTimeout here and then report a timeout that named no step. Measured live 2026-08-04: the
// activity itself is ~12ms with the ledger at seq ~8800, so any second-scale time spent in here is the
// substrate, and the caller must be able to say so and give up.
func (s *LedgerStore) PersistContext(ctx context.Context, e audit.LedgerEntry) error {
	_, err := s.p.Exec(ctx, `
		INSERT INTO governance_ledger (seq, decision, reason, action_id, withheld, prev_hash, hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		e.Seq, e.Decision, e.Reason, e.ActionID, e.Withheld, e.PrevHash, e.Hash)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// The seq PRIMARY KEY already exists: a sibling writer (the co-restarted worker seeded from the
			// same tail, a repair/restamp tool) advanced the shared head under our cached tail. Map to the
			// domain sentinel so the Ledger re-reads the head and re-chains (TG-549) instead of wedging its
			// governance lane. This is NEVER a silent overwrite — the existing audit row is untouched.
			return fmt.Errorf("%w: seq %d", audit.ErrDuplicateSeq, e.Seq)
		}
		return fmt.Errorf("db: persist ledger seq %d: %w", e.Seq, err)
	}
	return nil
}

// Persist is the deadline-less audit.LedgerSink arm, kept so every existing caller of Ledger.Append is
// byte-for-byte unchanged.
func (s *LedgerStore) Persist(e audit.LedgerEntry) error {
	return s.PersistContext(context.Background(), e)
}

// Tail returns the last (seq, hash) so a restarted worker continues the chain (audit.NewLedgerFromTail).
// (0, "", nil) when the ledger is empty.
func (s *LedgerStore) Tail(ctx context.Context) (int64, string, error) {
	var (
		seq  int64
		hash string
	)
	err := s.p.QueryRow(ctx, "SELECT seq, hash FROM governance_ledger ORDER BY seq DESC LIMIT 1").Scan(&seq, &hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, "", nil
		}
		return 0, "", fmt.Errorf("db: ledger tail: %w", err)
	}
	return seq, hash, nil
}

// All reads the entire chain in order for verification (audit.VerifyChain). Intended for an audit/read pass,
// not the hot path.
func (s *LedgerStore) All(ctx context.Context) ([]audit.LedgerEntry, error) {
	// created_at is projected for the operator surfaces (the console's ledger TIME column read blank for
	// every row until it was selected here). It is NOT part of the chain — see audit.LedgerEntry.CreatedAt
	// — so it is appended after the hashed columns rather than folded among them.
	rows, err := s.p.Query(ctx,
		"SELECT seq, decision, reason, action_id, withheld, prev_hash, hash, created_at FROM governance_ledger ORDER BY seq")
	if err != nil {
		return nil, fmt.Errorf("db: ledger all: %w", err)
	}
	defer rows.Close()
	var out []audit.LedgerEntry
	for rows.Next() {
		var e audit.LedgerEntry
		if err := rows.Scan(&e.Seq, &e.Decision, &e.Reason, &e.ActionID, &e.Withheld, &e.PrevHash, &e.Hash,
			&e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
