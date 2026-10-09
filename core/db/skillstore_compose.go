package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/territory-grounder/grounder/core/schema"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// ProductionRows is the composer's one-read snapshot (spec/014 REQ-1303): every production version
// joined with its skill identity, in deterministic compose order.
func (s *SkillStore) ProductionRows(ctx context.Context) ([]skillstore.ProductionRow, error) {
	// TG-489: the corpus proves itself before anything store-backed composes. A non-OK report —
	// tampered row, forged row, stale head, uninitialized chain — refuses the WHOLE snapshot; the
	// composer then falls back to the compiled registry IN FULL, visibly (composeGuidance records
	// the fallback reason in the session's skill_load provenance).
	rep, err := s.VerifyChainRead(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: production rows: %w", err)
	}
	if !rep.OK {
		return nil, fmt.Errorf("db: production rows REFUSED — %s", rep)
	}
	rows, err := s.p.Query(ctx, `
		SELECT v.id, v.skill_name, v.version, v.body, v.applies_when, v.content_hash, k.pinned, k.artifact_class, k.position
		FROM skill_version v
		JOIN skill k ON k.name = v.skill_name
		WHERE v.status = 'production'
		ORDER BY k.position ASC, v.skill_name ASC`)
	if err != nil {
		return nil, fmt.Errorf("db: production rows: %w", err)
	}
	defer rows.Close()
	var out []skillstore.ProductionRow
	for rows.Next() {
		var r skillstore.ProductionRow
		var aw []byte
		if err := rows.Scan(&r.VersionID, &r.SkillName, &r.Version, &r.Body, &aw, &r.ContentHash, &r.Pinned, &r.Class, &r.Position); err != nil {
			return nil, fmt.Errorf("db: scan production row: %w", err)
		}
		if len(aw) > 0 {
			if err := json.Unmarshal(aw, &r.AppliesWhen); err != nil {
				return nil, fmt.Errorf("db: applies_when for %s: %w", r.SkillName, err)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ImportCompiledVersion idempotently seeds one compiled skill as a PRODUCTION row (spec/014 REQ-1304:
// the console shows the real library from first start). Idempotency is SQL-side: the insert is skipped
// when the skill already has ANY production version (a graduated store row must never be displaced by a
// boot re-import) or when this exact (skill, version) row already exists.
func (s *SkillStore) ImportCompiledVersion(ctx context.Context, skillName, version, body string, aw skillstore.AppliesWhen) error {
	sv, err := schema.Stamp(schema.TableSkillVersion)
	if err != nil {
		return err
	}
	awJSON, err := json.Marshal(aw)
	if err != nil {
		return fmt.Errorf("db: marshal applies_when: %w", err)
	}
	tx, err := s.p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: import compiled begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Chain discipline (TG-489): head locked before the conditional insert; a skipped import commits
	// nothing and appends nothing.
	headLink, _, found, err := selectChainHead(ctx, tx, true)
	if err != nil {
		return err
	}
	if !found {
		return ErrChainUninitialized
	}
	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO skill_version
			(skill_name, version, status, body, applies_when, content_hash, author, source, rationale, schema_version)
		SELECT $1, $2, 'production', $3, $4, $5, 'compiled', 'compiled-import', '[production] compiled registry boot import', $6
		WHERE NOT EXISTS (SELECT 1 FROM skill_version WHERE skill_name = $1 AND status = 'production')
		ON CONFLICT (skill_name, version) DO NOTHING
		RETURNING id`,
		skillName, version, body, awJSON, skillstore.ContentHash(body, aw), int(sv)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Nothing inserted (a production row already exists, or this exact version row does) —
		// idempotent skip, no chain movement.
		return tx.Commit(ctx)
	}
	if err != nil {
		// A unique-violation is a CONCURRENT boot winning the same import (mid-deploy overlap of two
		// worker versions can race the NOT EXISTS check): the partial index keeps exactly one
		// production row, and the newest worker's supersede pass converges the version on its next
		// boot — benign, idempotent-by-outcome.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil
		}
		return fmt.Errorf("db: import compiled %s v%s: %w", skillName, version, err)
	}
	if err := chainAppend(ctx, tx, headLink, skillstore.ChainFacts{
		ID: id, SkillName: skillName, Version: version,
		ContentHash: skillstore.ContentHash(body, aw),
		Author:      "compiled", Source: "compiled-import",
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: import compiled commit: %w", err)
	}
	return nil
}
