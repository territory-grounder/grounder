package db

import (
	"context"
	"fmt"
	"time"
)

// EstateObjectGroupRow is one operator-authored OBJECT GROUP (estate_object_group, migration 0099): a named
// set of HOST-GLOB patterns whose membership is UNIONED into credential.Target.Groups alongside the
// sync-derived membership (TG-481, spec/016 — the shared object-group model the console editor and the
// policy engine both consume). Host-glob is the ONLY pattern kind end-to-end: the schema has no kind
// column, the write lane validates none, and the resolver matches host names only — the migration header's
// "host-glob / device-class" phrasing was aspiration, not schema, and it misled two planning passes
// (2026-08-22 TG-481 finding). A device-class lane would need a kind dimension end-to-end PLUS a per-host
// class source that does not exist (no DeviceClassSource analog to credential.MembershipSource) — an
// owner-ruled adopt-or-retire, not an implied backlog item. Precedence is per-group ('union' today: a
// hand-authored group ADDS to inventory-derived membership, never masks it). It is mutable operator
// CONFIG, not the audit spine — every write is ledgered by the worker lane before it lands here (mirrors
// credential_native_rule).
type EstateObjectGroupRow struct {
	ID         int64
	Name       string
	Patterns   []string
	Precedence string
	CreatedBy  string
	CreatedAt  time.Time
}

// EstateObjectGroupStore is the pgx-backed store for operator-authored object groups. Deliberately narrow:
// List for the resolution source + the console read; Insert/Delete for the single-writer worker lane.
// Parameters are always bound ($1) — no string-built SQL.
type EstateObjectGroupStore struct{ p *Pool }

// NewEstateObjectGroupStore returns the Postgres-backed object-group store.
func NewEstateObjectGroupStore(p *Pool) *EstateObjectGroupStore { return &EstateObjectGroupStore{p: p} }

// List returns every object group ordered by id (insertion order — stable for the console and the resolution
// source's row-addressed reporting).
func (s *EstateObjectGroupStore) List(ctx context.Context) ([]EstateObjectGroupRow, error) {
	rows, err := s.p.Pool.Query(ctx, `
		SELECT id, name, patterns, precedence, created_by, created_at
		FROM estate_object_group
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("db: estate_object_group list: %w", err)
	}
	defer rows.Close()
	var out []EstateObjectGroupRow
	for rows.Next() {
		var r EstateObjectGroupRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Patterns, &r.Precedence, &r.CreatedBy, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: estate_object_group scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: estate_object_group rows: %w", err)
	}
	return out, nil
}

// Insert appends one validated object group and returns its id. The caller (the worker lane) has already
// validated the name/patterns and appended the governance record — this is the persist half only.
func (s *EstateObjectGroupStore) Insert(ctx context.Context, name string, patterns []string, precedence, createdBy string) (int64, error) {
	var id int64
	err := s.p.Pool.QueryRow(ctx, `
		INSERT INTO estate_object_group (name, patterns, precedence, created_by)
		VALUES ($1, $2, $3, $4)
		RETURNING id`, name, patterns, precedence, createdBy).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("db: estate_object_group insert: %w", err)
	}
	return id, nil
}

// Delete removes one object group by id. It reports (false, nil) when no such row exists — the caller maps
// that to its typed not-found refusal — and errors only on a genuine database failure.
func (s *EstateObjectGroupStore) Delete(ctx context.Context, id int64) (bool, error) {
	tag, err := s.p.Pool.Exec(ctx, `DELETE FROM estate_object_group WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("db: estate_object_group delete (id %d): %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}
