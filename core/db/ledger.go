package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/audit"
)

// LedgerStore is the pgx-backed, append-only writer for governance_ledger (migration 0003). It implements
// audit.LedgerSink, so an in-memory audit.Ledger mirrors every appended entry to Postgres write-through. The
// hash chain is computed by the Ledger (the DB stores the already-linked rows); a post-hoc DB edit is caught
// by audit.VerifyChain over All().
type LedgerStore struct{ p *Pool }

// NewLedgerStore returns a Postgres-backed governance-ledger writer.
func NewLedgerStore(p *Pool) *LedgerStore { return &LedgerStore{p: p} }

var _ audit.LedgerSink = (*LedgerStore)(nil)

// Persist appends one hash-chained governance entry. seq is the chain position supplied by the Ledger (the
// governance_ledger PK), so a duplicate seq is a hard error — never a silent overwrite of an audit row.
func (s *LedgerStore) Persist(e audit.LedgerEntry) error {
	_, err := s.p.Exec(context.Background(), `
		INSERT INTO governance_ledger (seq, decision, reason, action_id, withheld, prev_hash, hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		e.Seq, e.Decision, e.Reason, e.ActionID, e.Withheld, e.PrevHash, e.Hash)
	if err != nil {
		return fmt.Errorf("db: persist ledger seq %d: %w", e.Seq, err)
	}
	return nil
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
	rows, err := s.p.Query(ctx,
		"SELECT seq, decision, reason, action_id, withheld, prev_hash, hash FROM governance_ledger ORDER BY seq")
	if err != nil {
		return nil, fmt.Errorf("db: ledger all: %w", err)
	}
	defer rows.Close()
	var out []audit.LedgerEntry
	for rows.Next() {
		var e audit.LedgerEntry
		if err := rows.Scan(&e.Seq, &e.Decision, &e.Reason, &e.ActionID, &e.Withheld, &e.PrevHash, &e.Hash); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
