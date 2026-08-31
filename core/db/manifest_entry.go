package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/worldmodel"
)

// WorldManifestStore is the pgx persistence for the auto-drafted WORLD MODEL (spec/027 REQ-2702, migration
// 0047). The name disambiguates it from db.ManifestStore, which is the unrelated action_manifest writer
// (INV-07 content-hashed action bindings) — "manifest" is overloaded in this repo, and two stores with one
// name would be a genuine hazard at the composition root. It is deliberately thin: the state machine, the mandatory rationale, and the ledger-before-row
// ordering all live in core/worldmodel.Transition, which drives this store — so there is exactly one place
// a status can change and this file cannot become a second one.
type WorldManifestStore struct{ p *Pool }

// NewWorldManifestStore builds the store over a pool.
func NewWorldManifestStore(p *Pool) *WorldManifestStore { return &WorldManifestStore{p: p} }

// UpdateEntry persists a transitioned entry. It writes ONLY the fields Transition owns — status, the
// append-only rationale log, the server-derived approver, the ledger seq, the status clock, and the
// ratcheted confidence — so a concurrent discovery pass updating last_seen_at cannot be clobbered by an
// adoption, and vice versa.
func (s *WorldManifestStore) UpdateEntry(ctx context.Context, e worldmodel.Entry) error {
	if s == nil || s.p == nil {
		return errors.New("manifest store: no pool")
	}
	_, err := s.p.Exec(ctx, `
		UPDATE manifest_entry
		   SET status = $1, rationale = $2, approver = $3, ledger_seq = $4,
		       status_changed_at = $5,
		       -- MAX-ratchet in SQL too (REQ-2706): a write can only ever RAISE confidence, so a racing
		       -- weaker-source sighting cannot lower what a stronger source established.
		       confidence = GREATEST(confidence, $6)
		 WHERE entity_type = $7 AND name = $8`,
		string(e.Status), e.Rationale, e.Approver, e.LedgerSeq,
		e.StatusChangedAt, e.Confidence, string(e.EntityType), e.Name)
	if err != nil {
		return fmt.Errorf("manifest update: %w", err)
	}
	return nil
}

// ApprovedEntries returns every entry currently materializing into the allowlist union — approved AND
// stale. Stale rows are INCLUDED deliberately: discovery losing sight of a unit must never silently narrow
// an operator's grant (REQ-2705, the safe direction). Only an explicit retire removes an entry from here.
func (s *WorldManifestStore) ApprovedEntries(ctx context.Context) ([]worldmodel.Entry, error) {
	if s == nil || s.p == nil {
		return nil, errors.New("manifest store: no pool")
	}
	rows, err := s.p.Query(ctx, `
		SELECT id, entity_type, name, host, source, confidence, status,
		       approver, ledger_seq, status_changed_at
		  FROM manifest_entry
		 WHERE status IN ('approved','retired_candidate_stale')
		 ORDER BY entity_type, name`)
	if err != nil {
		return nil, fmt.Errorf("manifest approved: %w", err)
	}
	defer rows.Close()

	var out []worldmodel.Entry
	for rows.Next() {
		var e worldmodel.Entry
		var typ, src, status string
		if err := rows.Scan(&e.ID, &typ, &e.Name, &e.Host, &src, &e.Confidence, &status,
			&e.Approver, &e.LedgerSeq, &e.StatusChangedAt); err != nil {
			return nil, fmt.Errorf("manifest scan: %w", err)
		}
		e.EntityType = estate.EntityType(typ)
		e.Source = estate.Source(src)
		e.Status = worldmodel.Status(status)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("manifest rows: %w", err)
	}
	return out, nil
}

// DraftEntry records a discovered fact as a DRAFT — the state that grants nothing. It is the ONLY writer
// that creates rows, and it can never create an approved one: the status is not a parameter.
//
// A re-sighting refreshes last_seen_at and ratchets confidence upward, but NEVER resets an entry's status —
// re-discovering an adopted unit must not un-adopt it, and re-discovering a rejected one must not resurrect
// it (the no-resurrection law lives in the state machine; this is its persistence-layer counterpart).
func (s *WorldManifestStore) DraftEntry(ctx context.Context, e worldmodel.Entry) error {
	if s == nil || s.p == nil {
		return errors.New("manifest store: no pool")
	}
	_, err := s.p.Exec(ctx, `
		INSERT INTO manifest_entry (entity_type, name, host, source, confidence, status)
		VALUES ($1, $2, $3, $4, $5, 'draft')
		ON CONFLICT (entity_type, name) WHERE status NOT IN ('rejected','retired')
		DO UPDATE SET last_seen_at = now(),
		              confidence   = GREATEST(manifest_entry.confidence, EXCLUDED.confidence)`,
		string(e.EntityType), e.Name, e.Host, string(e.Source), e.Confidence)
	if err != nil {
		return fmt.Errorf("manifest draft: %w", err)
	}
	return nil
}

// compile-time proof the pgx store satisfies the chokepoint's persistence contract.
var _ worldmodel.Store = (*WorldManifestStore)(nil)

var _ = pgx.ErrNoRows // keep the pgx import meaningful across build tags

// AllEntries returns the reviewable set — every entry regardless of status, newest activity first — plus
// the REAL draft and total counts. The counts are computed by the DATABASE over the whole table, never
// from len(page): a badge that silently became "page size" once the estate grew past the limit is exactly
// the fabricated-number class INV-15 bans, and this surface's whole job is to tell an operator how much
// is waiting for them.
func (s *WorldManifestStore) AllEntries(ctx context.Context, limit int) ([]worldmodel.Entry, int, int, error) {
	if s == nil || s.p == nil {
		return nil, 0, 0, errors.New("manifest store: no pool")
	}
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	var drafts, total int
	if err := s.p.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'draft'), count(*) FROM manifest_entry`).
		Scan(&drafts, &total); err != nil {
		return nil, 0, 0, fmt.Errorf("manifest counts: %w", err)
	}
	rows, err := s.p.Query(ctx, `
		SELECT id, entity_type, name, host, source, confidence, status,
		       approver, ledger_seq, first_seen_at, last_seen_at, status_changed_at
		  FROM manifest_entry
		 ORDER BY (status = 'draft') DESC, last_seen_at DESC NULLS LAST, entity_type, name
		 LIMIT $1`, limit)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("manifest list: %w", err)
	}
	defer rows.Close()
	var out []worldmodel.Entry
	for rows.Next() {
		var e worldmodel.Entry
		var typ, src, status string
		if err := rows.Scan(&e.ID, &typ, &e.Name, &e.Host, &src, &e.Confidence, &status,
			&e.Approver, &e.LedgerSeq, &e.FirstSeenAt, &e.LastSeenAt, &e.StatusChangedAt); err != nil {
			return nil, 0, 0, fmt.Errorf("manifest scan: %w", err)
		}
		e.EntityType = estate.EntityType(typ)
		e.Source = estate.Source(src)
		e.Status = worldmodel.Status(status)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, fmt.Errorf("manifest rows: %w", err)
	}
	return out, drafts, total, nil
}

// EntryByID loads one reviewable row for a transition. found=false is an honest "no such entry" — the
// write lane turns it into a 404 rather than inventing a row to refuse.
func (s *WorldManifestStore) EntryByID(ctx context.Context, id int64) (worldmodel.Entry, bool, error) {
	if s == nil || s.p == nil {
		return worldmodel.Entry{}, false, errors.New("manifest store: no pool")
	}
	var e worldmodel.Entry
	var typ, src, status string
	err := s.p.QueryRow(ctx, `
		SELECT id, entity_type, name, host, source, confidence, status,
		       COALESCE(rationale,''), approver, ledger_seq
		  FROM manifest_entry WHERE id = $1`, id).
		Scan(&e.ID, &typ, &e.Name, &e.Host, &src, &e.Confidence, &status,
			&e.Rationale, &e.Approver, &e.LedgerSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return worldmodel.Entry{}, false, nil
	}
	if err != nil {
		return worldmodel.Entry{}, false, fmt.Errorf("manifest by id: %w", err)
	}
	e.EntityType = estate.EntityType(typ)
	e.Source = estate.Source(src)
	e.Status = worldmodel.Status(status)
	return e, true, nil
}
