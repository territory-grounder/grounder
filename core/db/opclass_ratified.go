package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
)

// OpClassRatifiedStore is the pgx surface over opclass_ratified (migration 0049, spec/028 REQ-2803,
// ADR-0016): the APPEND-ONLY earned op-class overlay.
//
// Append-only is not a convention here, it is a privilege: the migration REVOKEs UPDATE and DELETE from
// tg_runtime, so this store CANNOT edit or erase a grant even if a future caller asked it to. A revocation
// is therefore a new row carrying revoked=true, and the partial unique index
// (op_class WHERE NOT revoked) makes "at most one live grant per class" a database fact rather than a
// query someone has to get right. The capability history is the table; nothing can rewrite it.
//
// The rows this store returns feed opschema.SetOverlay, which re-verifies every entry_hash and DROPS a
// mismatching row. That is why the store does no trust-checking of its own: a store that tried to decide
// which rows are legitimate would be a second, weaker copy of the verification that already happens at the
// composition seam — and the only safe reading of unprovable provenance is absence, which SetOverlay
// already implements.
type OpClassRatifiedStore struct{ p *Pool }

// NewOpClassRatifiedStore returns the Postgres-backed overlay store.
func NewOpClassRatifiedStore(p *Pool) *OpClassRatifiedStore { return &OpClassRatifiedStore{p: p} }

// RatifiedRow is one overlay row as the lane and the loader consume it.
type RatifiedRow struct {
	OpClass          string
	Seq              int64
	Spec             opschema.OpClassSpec
	EntryHash        string
	Family           string
	Tier             string
	PromoteThreshold int
	Revoked          bool
	CandidateKey     string
	Approver         string
	Rationale        string
	LedgerSeq        int64
}

// Ratify appends the grant row for an operator-authored spec.
//
// The caller has already validated the spec through opschema.ValidateRatification (the admission gate,
// including the laundering tripwire) and appended the opclass:ratify GovDecision whose action_id is
// entryHash — so the row's CONTENT is chain-covered, not merely its existence. This method's whole job is
// to persist that decision durably and let the unique index refuse a second live grant.
func (s *OpClassRatifiedStore) Ratify(ctx context.Context, r RatifiedRow) (int64, error) {
	specJSON, err := json.Marshal(r.Spec)
	if err != nil {
		return 0, fmt.Errorf("canonicalize ratified spec: %w", err)
	}
	var seq int64
	err = s.p.QueryRow(ctx, `
		INSERT INTO opclass_ratified
		  (op_class, spec, entry_hash, family, tier, promote_threshold, revoked,
		   candidate_key, approver, rationale, ledger_seq)
		VALUES ($1, $2, $3, $4, $5, $6, false, $7, $8, $9, $10)
		RETURNING seq`,
		r.OpClass, specJSON, r.EntryHash, r.Family, r.Tier, r.PromoteThreshold,
		r.CandidateKey, r.Approver, r.Rationale, r.LedgerSeq).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("ratify %q: %w", r.OpClass, err)
	}
	return seq, nil
}

// Revoke appends a REVOCATION row — it does not update the grant.
//
// This is the shape the append-only privilege forces, and it is the better shape anyway: after a revocation
// the table still shows what was granted, by whom, on what rationale, and when it was withdrawn. An UPDATE
// would have destroyed exactly the evidence an incident review needs.
//
// The new row carries the same entry_hash so the pair is joinable, revoked=true so the partial unique index
// frees the slug for a future grant, and its own ledger_seq — the opclass:revoke decision, which is a
// separate governed act from the ratify it withdraws.
func (s *OpClassRatifiedStore) Revoke(ctx context.Context, opClass, approver, rationale string, ledgerSeq int64) error {
	ct, err := s.p.Exec(ctx, `
		INSERT INTO opclass_ratified
		  (op_class, spec, entry_hash, family, tier, promote_threshold, revoked,
		   candidate_key, approver, rationale, ledger_seq)
		SELECT op_class, spec, entry_hash, family, tier, promote_threshold, true,
		       candidate_key, $2, $3, $4
		  FROM opclass_ratified
		 WHERE op_class = $1 AND NOT revoked`,
		opClass, approver, rationale, ledgerSeq)
	if err != nil {
		return fmt.Errorf("revoke %q: %w", opClass, err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("revoke %q: no live grant to withdraw", opClass)
	}
	return nil
}

// IsLive reports whether the class currently holds a grant.
//
// It exists so the verbs that act on a GRANT (revoke, export-embed) can refuse with an honest 409 before
// doing anything, rather than discovering absence halfway through and leaving a ledger row describing a
// withdrawal that never happened.
func (s *OpClassRatifiedStore) IsLive(ctx context.Context, opClass string) (bool, error) {
	var live bool
	err := s.p.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM opclass_ratified WHERE op_class = $1 AND NOT revoked)`, opClass).Scan(&live)
	if err != nil {
		return false, fmt.Errorf("live grant check %q: %w", opClass, err)
	}
	return live, nil
}

// LiveOverlay returns every currently-granted row, oldest first, for the composed registry to load.
//
// Revoked rows are excluded HERE rather than filtered by the caller: the loader's job is to verify hashes,
// not to remember a WHERE clause. A revoked class must reach rung 0 (registry absence) the moment its
// revocation lands, and the cheapest way to guarantee that is for it never to appear in this result.
func (s *OpClassRatifiedStore) LiveOverlay(ctx context.Context) ([]RatifiedRow, error) {
	rows, err := s.p.Query(ctx, `
		SELECT op_class, seq, spec, entry_hash, family, tier, promote_threshold,
		       candidate_key, approver, rationale, ledger_seq
		  FROM opclass_ratified
		 WHERE NOT revoked
		 ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("live overlay: %w", err)
	}
	defer rows.Close()
	var out []RatifiedRow
	for rows.Next() {
		var r RatifiedRow
		var specJSON []byte
		if err := rows.Scan(&r.OpClass, &r.Seq, &specJSON, &r.EntryHash, &r.Family, &r.Tier,
			&r.PromoteThreshold, &r.CandidateKey, &r.Approver, &r.Rationale, &r.LedgerSeq); err != nil {
			return nil, fmt.Errorf("live overlay scan: %w", err)
		}
		// A row whose spec will not decode is DROPPED, not repaired and not fatal: one corrupt row must
		// not deny the whole overlay (which would revoke every other earned class at once), and a
		// half-decoded spec must never reach the registry. Same failure direction as the hash check.
		if err := json.Unmarshal(specJSON, &r.Spec); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OverlayEntries adapts the live rows into the loader's shape. The hash travels WITH the spec so
// SetOverlay can recompute and compare — the store asserts nothing about validity.
func (s *OpClassRatifiedStore) OverlayEntries(ctx context.Context) ([]opschema.OverlayEntry, error) {
	rows, err := s.LiveOverlay(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]opschema.OverlayEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, opschema.OverlayEntry{Spec: r.Spec, Hash: r.EntryHash})
	}
	return out, nil
}
