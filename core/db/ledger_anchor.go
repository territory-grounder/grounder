package db

import (
	"context"
	"fmt"

	"github.com/territory-grounder/grounder/core/audit"
)

// Head reads the governance_ledger HEAD (max seq and its chain hash) plus the last `window` rows — the input
// core/audit.ComputeAnchor folds into a HEAD anchor (TG-80 P1#1). An EMPTY ledger returns the zero HeadState
// (Seq 0), which the recorder reads as "nothing to witness yet" rather than anchoring a phantom fixed point.
//
// It reads only (seq, hash): never `reason`/`decision`, so a HEAD read can never itself surface ledger free
// text. Bound query, no interpolation. The DESCending LIMIT bounds the read to the window (the ledger runs to
// thousands of rows); the rows are reversed into ascending order so Recent[last] is the HEAD.
func (s *LedgerStore) Head(ctx context.Context, window int) (audit.HeadState, error) {
	if window <= 0 {
		window = audit.DefaultAnchorWindow
	}
	rows, err := s.p.Query(ctx, `SELECT seq, hash FROM governance_ledger ORDER BY seq DESC LIMIT $1`, window)
	if err != nil {
		return audit.HeadState{}, fmt.Errorf("db: ledger head: %w", err)
	}
	defer rows.Close()
	var desc []audit.RowRef
	for rows.Next() {
		var r audit.RowRef
		if err := rows.Scan(&r.Seq, &r.Hash); err != nil {
			return audit.HeadState{}, err
		}
		desc = append(desc, r)
	}
	if err := rows.Err(); err != nil {
		return audit.HeadState{}, err
	}
	if len(desc) == 0 {
		return audit.HeadState{}, nil // empty ledger — Seq 0
	}
	recent := make([]audit.RowRef, len(desc))
	for i, r := range desc {
		recent[len(desc)-1-i] = r // reverse DESC -> ASC
	}
	head := recent[len(recent)-1]
	return audit.HeadState{Seq: head.Seq, Hash: head.Hash, Recent: recent}, nil
}

// AnchorStore is the pgx-backed, append-only writer/reader for ledger_anchor (migration 0092): the durable
// witness history of the governance_ledger HEAD. The recording principal (tg_runtime) holds INSERT + SELECT
// but NOT UPDATE/DELETE on this table — so, like the spine it witnesses, an anchor once recorded cannot be
// rewritten or erased by the process that wrote it, which is the property that makes the witness meaningful.
type AnchorStore struct{ p *Pool }

// NewAnchorStore returns a Postgres-backed ledger-anchor writer/reader.
func NewAnchorStore(p *Pool) *AnchorStore { return &AnchorStore{p: p} }

// Record appends one HEAD anchor for a named chain DOMAIN (TG-515: 'governance-ledger', or a second consumer
// like TG-510's knowledge corpus). `id` and `created_at` are assigned by the table (identity + DEFAULT now()),
// so this writes only the domain + witnessed content. Bound params, no interpolation (INV-03).
func (s *AnchorStore) Record(ctx context.Context, domain string, a audit.Anchor) error {
	_, err := s.p.Exec(ctx, `
		INSERT INTO ledger_anchor (domain, anchored_seq, anchored_hash, window_size, digest)
		VALUES ($1, $2, $3, $4, $5)`,
		domain, a.Seq, a.Hash, a.WindowSize, a.Digest)
	if err != nil {
		return fmt.Errorf("db: record %s anchor at seq %d: %w", domain, a.Seq, err)
	}
	return nil
}

// Anchors reads ONE domain's witness history in record order (oldest first) for core/audit.VerifyAgainstAnchors.
// Filtering by domain is what keeps one chain's witnesses from ever being checked against another's chain.
func (s *AnchorStore) Anchors(ctx context.Context, domain string) ([]audit.Anchor, error) {
	rows, err := s.p.Query(ctx,
		`SELECT anchored_seq, anchored_hash, window_size, digest, created_at FROM ledger_anchor WHERE domain = $1 ORDER BY id`, domain)
	if err != nil {
		return nil, fmt.Errorf("db: read %s anchors: %w", domain, err)
	}
	defer rows.Close()
	var out []audit.Anchor
	for rows.Next() {
		var a audit.Anchor
		if err := rows.Scan(&a.Seq, &a.Hash, &a.WindowSize, &a.Digest, &a.At); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Scoped binds a domain so the recorder + verifier for ONE chain keep the plain (ctx, a) / (ctx) calls that
// temporal/ledgeranchor's Job (recorder) and VerifyJob (checker) expect, while the store itself stays a single
// domain-keyed table. The governance wiring is s.Scoped(audit.DomainGovernanceLedger); TG-510 uses its own.
func (s *AnchorStore) Scoped(domain string) *ScopedAnchorStore { return &ScopedAnchorStore{s: s, domain: domain} }

// ScopedAnchorStore is a domain-bound view of AnchorStore — the same append-only writer/reader, fixed to one
// chain's domain so it satisfies the domain-agnostic recorder/verifier interfaces.
type ScopedAnchorStore struct {
	s      *AnchorStore
	domain string
}

// Record appends a HEAD anchor for the bound domain.
func (v *ScopedAnchorStore) Record(ctx context.Context, a audit.Anchor) error {
	return v.s.Record(ctx, v.domain, a)
}

// Anchors reads the bound domain's witness history.
func (v *ScopedAnchorStore) Anchors(ctx context.Context) ([]audit.Anchor, error) {
	return v.s.Anchors(ctx, v.domain)
}
