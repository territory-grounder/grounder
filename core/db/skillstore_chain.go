package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/skillstore"
)

// The distillate-corpus tamper chain, DB side (TG-489; owner ruling TG-488 B24 / TG-146 S6).
// Pure link/verify logic lives in core/skillstore/chain.go; this file does the row I/O:
//
//   - EnsureChain     — boot-time initialization: backfills links for pre-chain rows and creates the
//                       singleton head row. It NEVER heals: an existing link that fails recomputation
//                       aborts initialization loudly (tamper or algorithm drift — an operator decides).
//   - VerifyChainRead — one consistent snapshot of every row + the head, verified from genesis.
//   - chained appends — CreateVersion and ImportCompiledVersion run inside a transaction that locks
//                       the head row FOR UPDATE before inserting, so append order and bigserial id
//                       order cannot diverge, then links the new row and advances the head.
//
// ProductionRows (the composer's snapshot) verifies FIRST and refuses store-backed content on any
// non-OK report — composition then falls back to the compiled registry IN FULL, visibly, exactly like
// any other store failure (temporal/runner/compose_seed.go composeGuidance).

// ErrChainUninitialized refuses a chained append before EnsureChain has created the head row.
var ErrChainUninitialized = errors.New(
	"db: distillate chain uninitialized — EnsureChain must run first (worker boot does this)")

// chainAdvisoryLockID serializes concurrent EnsureChain bootstraps across processes (two workers
// booting after the 0094 deploy). Arbitrary constant, unique within TG's advisory-lock usage.
const chainAdvisoryLockID = 76474889

// chainQuerier is the subset of pgx both a pool and a transaction satisfy — the chain reads run
// against either.
type chainQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// selectChainRows reads every skill_version row's chain-bound facts in append (id) order. The
// content hash is RECOMPUTED from the stored body and predicate — never trusted from the content_hash
// column — so a body edit breaks the chain even when that column was edited to match.
func selectChainRows(ctx context.Context, q chainQuerier) ([]skillstore.ChainRow, error) {
	rows, err := q.Query(ctx, `
		SELECT v.id, v.skill_name, v.version, v.body, v.applies_when, v.author, v.source,
		       COALESCE(v.parent_version_id, 0), COALESCE(v.chain_link, '')
		FROM skill_version v
		ORDER BY v.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("db: chain rows: %w", err)
	}
	defer rows.Close()
	var out []skillstore.ChainRow
	for rows.Next() {
		var (
			r    skillstore.ChainRow
			body string
			aw   []byte
		)
		if err := rows.Scan(&r.Facts.ID, &r.Facts.SkillName, &r.Facts.Version, &body, &aw,
			&r.Facts.Author, &r.Facts.Source, &r.Facts.ParentVersionID, &r.StoredLink); err != nil {
			return nil, fmt.Errorf("db: scan chain row: %w", err)
		}
		var pred skillstore.AppliesWhen
		if len(aw) > 0 {
			if err := json.Unmarshal(aw, &pred); err != nil {
				return nil, fmt.Errorf("db: chain applies_when for row %d: %w", r.Facts.ID, err)
			}
		}
		r.Facts.ContentHash = skillstore.ContentHash(body, pred)
		out = append(out, r)
	}
	return out, rows.Err()
}

// selectChainHead reads the singleton head row. found=false means the chain is uninitialized.
func selectChainHead(ctx context.Context, q chainQuerier, forUpdate bool) (head string, count int64, found bool, err error) {
	sql := `SELECT head, row_count FROM skill_chain_head`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	err = q.QueryRow(ctx, sql).Scan(&head, &count)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("db: chain head: %w", err)
	}
	return head, count, true, nil
}

// VerifyChainRead verifies the whole corpus chain in ONE consistent snapshot. REPEATABLE READ is
// load-bearing, not decoration (TG-489 review finding #1): AccessMode alone leaves Postgres at
// READ COMMITTED, where EACH STATEMENT gets its own MVCC snapshot — a concurrent append committing
// between the rows read and the head read would then manufacture a spurious count/head mismatch
// and needlessly compile-fallback a live session (fail-closed, but a false alarm). Under
// REPEATABLE READ both selects share the transaction's first snapshot, so a concurrent append is
// either wholly visible or wholly invisible. The report always carries its denominator; an
// uninitialized chain is its own named state.
func (s *SkillStore) VerifyChainRead(ctx context.Context) (skillstore.ChainReport, error) {
	tx, err := s.p.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return skillstore.ChainReport{}, fmt.Errorf("db: chain verify begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := selectChainRows(ctx, tx)
	if err != nil {
		return skillstore.ChainReport{}, err
	}
	head, count, found, err := selectChainHead(ctx, tx, false)
	if err != nil {
		return skillstore.ChainReport{}, err
	}
	if !found {
		return skillstore.UninitializedChainReport(len(rows)), nil
	}
	return skillstore.VerifyChain(rows, head, count), nil
}

// EnsureChain initializes the chain exactly once: backfills chain_link for every pre-chain row (in id
// order, from genesis) and creates the head row. Idempotent and crash-resumable — a link already
// present must MATCH its recomputation or initialization ABORTS with the offending row named; healing
// a mismatched link here would launder exactly the tamper this chain exists to expose. When the head
// row already exists, EnsureChain initializes nothing and simply returns the verification report.
func (s *SkillStore) EnsureChain(ctx context.Context) (skillstore.ChainReport, error) {
	tx, err := s.p.Begin(ctx)
	if err != nil {
		return skillstore.ChainReport{}, fmt.Errorf("db: chain init begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialize concurrent bootstraps (two processes booting after the 0094 deploy): the head row
	// does not exist yet, so there is nothing to FOR UPDATE — an advisory lock gates instead.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, chainAdvisoryLockID); err != nil {
		return skillstore.ChainReport{}, fmt.Errorf("db: chain init lock: %w", err)
	}
	if _, _, found, err := selectChainHead(ctx, tx, true); err != nil {
		return skillstore.ChainReport{}, err
	} else if found {
		_ = tx.Rollback(ctx)
		return s.VerifyChainRead(ctx)
	}
	rows, err := selectChainRows(ctx, tx)
	if err != nil {
		return skillstore.ChainReport{}, err
	}
	prev := skillstore.ChainGenesis
	for _, r := range rows {
		want := skillstore.ChainLink(prev, r.Facts)
		switch r.StoredLink {
		case "":
			if _, err := tx.Exec(ctx, `UPDATE skill_version SET chain_link = $1 WHERE id = $2`, want, r.Facts.ID); err != nil {
				return skillstore.ChainReport{}, fmt.Errorf("db: chain backfill row %d: %w", r.Facts.ID, err)
			}
		case want:
			// crash-resume: an earlier partial bootstrap already linked this row correctly.
		default:
			rep := skillstore.ChainReport{Total: len(rows), BrokenID: r.Facts.ID, Reason: skillstore.ChainReasonLinkMismatch}
			return rep, fmt.Errorf("db: chain init REFUSED — row id=%d carries a link that fails recomputation "+
				"(tamper or algorithm drift); EnsureChain never heals a mismatched link, investigate the row", r.Facts.ID)
		}
		prev = want
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO skill_chain_head (head, row_count) VALUES ($1, $2)`, prev, int64(len(rows))); err != nil {
		return skillstore.ChainReport{}, fmt.Errorf("db: chain head create: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return skillstore.ChainReport{}, fmt.Errorf("db: chain init commit: %w", err)
	}
	return s.VerifyChainRead(ctx)
}

// chainAppend links a just-inserted row and advances the head. It MUST run inside the same
// transaction as the insert, with the head row already locked FOR UPDATE before the insert (the lock
// is what serializes appends so id order and chain order agree). headLink is the locked head value.
func chainAppend(ctx context.Context, tx pgx.Tx, headLink string, f skillstore.ChainFacts) error {
	link := skillstore.ChainLink(headLink, f)
	tag, err := tx.Exec(ctx, `UPDATE skill_version SET chain_link = $1 WHERE id = $2`, link, f.ID)
	if err != nil {
		return fmt.Errorf("db: chain link row %d: %w", f.ID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("db: chain link row %d: %d rows affected", f.ID, tag.RowsAffected())
	}
	tag, err = tx.Exec(ctx,
		`UPDATE skill_chain_head SET head = $1, row_count = row_count + 1, updated_at = now()`, link)
	if err != nil {
		return fmt.Errorf("db: chain head advance: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("db: chain head advance: %d rows affected", tag.RowsAffected())
	}
	return nil
}
