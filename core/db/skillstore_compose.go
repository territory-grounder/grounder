package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/territory-grounder/grounder/core/schema"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// ProductionRows is the composer's one-read snapshot (spec/014 REQ-1303): every production version
// joined with its skill identity, in deterministic compose order.
func (s *SkillStore) ProductionRows(ctx context.Context) ([]skillstore.ProductionRow, error) {
	rows, err := s.p.Query(ctx, `
		SELECT v.id, v.skill_name, v.version, v.body, v.applies_when, v.content_hash, k.pinned, k.position
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
		if err := rows.Scan(&r.VersionID, &r.SkillName, &r.Version, &r.Body, &aw, &r.ContentHash, &r.Pinned, &r.Position); err != nil {
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
	_, err = s.p.Exec(ctx, `
		INSERT INTO skill_version
			(skill_name, version, status, body, applies_when, content_hash, author, source, rationale, schema_version)
		SELECT $1, $2, 'production', $3, $4, $5, 'compiled', 'compiled-import', '[production] compiled registry boot import', $6
		WHERE NOT EXISTS (SELECT 1 FROM skill_version WHERE skill_name = $1 AND status = 'production')
		ON CONFLICT (skill_name, version) DO NOTHING`,
		skillName, version, body, awJSON, skillstore.ContentHash(body, aw), int(sv))
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
	return nil
}
