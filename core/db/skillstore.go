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

// SkillStore is the pgx-backed skillstore.Store over skill / skill_version (migration 0009). The state
// machine and its invariants live in core/skillstore (Transition, ValidateDraft); this layer is pure
// row I/O — parameterized SQL only, structural invariants re-enforced by the schema's CHECK constraints
// and partial unique indexes. Integration-tested under compose (CI has no Postgres, constraint D5).
type SkillStore struct{ p *Pool }

// NewSkillStore returns a Postgres-backed skill store.
func NewSkillStore(p *Pool) *SkillStore { return &SkillStore{p: p} }

// PutSkill upserts a skill identity row (the boot importer's idempotent seed path).
func (s *SkillStore) PutSkill(ctx context.Context, sk skillstore.Skill) error {
	_, err := s.p.Exec(ctx, `
		INSERT INTO skill (name, kind, pinned, position) VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO UPDATE SET kind = EXCLUDED.kind, pinned = EXCLUDED.pinned, position = EXCLUDED.position`,
		sk.Name, sk.Kind, sk.Pinned, sk.Position)
	if err != nil {
		return fmt.Errorf("db: put skill %s: %w", sk.Name, err)
	}
	return nil
}

// CreateVersion validates and inserts a draft row.
func (s *SkillStore) CreateVersion(ctx context.Context, v skillstore.Version) (skillstore.Version, error) {
	if err := skillstore.ValidateDraft(ctx, s, v); err != nil {
		return skillstore.Version{}, err
	}
	sv, err := schema.Stamp(schema.TableSkillVersion)
	if err != nil {
		return skillstore.Version{}, err
	}
	aw, err := json.Marshal(v.AppliesWhen)
	if err != nil {
		return skillstore.Version{}, fmt.Errorf("db: marshal applies_when: %w", err)
	}
	var parent any
	if v.ParentVersionID > 0 {
		parent = v.ParentVersionID
	}
	err = s.p.QueryRow(ctx, `
		INSERT INTO skill_version
			(skill_name, version, status, body, applies_when, content_hash, author, source, rationale, parent_version_id, schema_version)
		VALUES ($1, $2, 'draft', $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, created_at, status_changed_at`,
		v.SkillName, v.Version, v.Body, aw, v.ContentHash, v.Author, v.Source, v.Rationale, parent, int(sv)).
		Scan(&v.ID, &v.CreatedAt, &v.StatusChangedAt)
	if err != nil {
		return skillstore.Version{}, fmt.Errorf("db: create skill version %s v%s: %w", v.SkillName, v.Version, err)
	}
	v.Status = skillstore.StatusDraft
	return v, nil
}

// versionCols are skill_version's columns, "v."-qualified so the same list works in the JOINed
// ProductionSet query (skill also carries a created_at — an unqualified list is ambiguous there,
// which CI cannot catch without Postgres).
// eval_offline is COALESCE'd to the jsonb literal 'null' so an absent eval scans as the 4 bytes "null"
// (treated as ABSENT below, never a fabricated object). It rides the version row so the flywheel's start
// phase can rank admitted candidates by their offline discovery-delta (TG-65 top-K arm cap) without a
// second query — the same source the console version-history read exposes.
const versionCols = `v.id, v.skill_name, v.version, v.status, v.body, v.applies_when, v.content_hash,
	v.author, v.source, v.rationale, COALESCE(v.parent_version_id, 0), COALESCE(v.ledger_seq, 0),
	v.created_at, v.status_changed_at, COALESCE(v.eval_offline, 'null')`

func scanVersion(row interface{ Scan(...any) error }) (skillstore.Version, error) {
	var v skillstore.Version
	var aw, evalOffline []byte
	if err := row.Scan(&v.ID, &v.SkillName, &v.Version, &v.Status, &v.Body, &aw, &v.ContentHash,
		&v.Author, &v.Source, &v.Rationale, &v.ParentVersionID, &v.LedgerSeq, &v.CreatedAt, &v.StatusChangedAt,
		&evalOffline); err != nil {
		return skillstore.Version{}, err
	}
	if len(aw) > 0 {
		if err := json.Unmarshal(aw, &v.AppliesWhen); err != nil {
			return skillstore.Version{}, fmt.Errorf("db: applies_when for version %d: %w", v.ID, err)
		}
	}
	// An absent offline eval is coalesced to jsonb 'null' at the query; keep OfflineEval nil in that case
	// so a never-scored version is not fabricated with a null blob (matches the MemStore fake).
	if len(evalOffline) > 0 && string(evalOffline) != "null" {
		v.OfflineEval = evalOffline
	}
	return v, nil
}

// GetVersion implements skillstore.Store.
func (s *SkillStore) GetVersion(ctx context.Context, id int64) (skillstore.Version, error) {
	v, err := scanVersion(s.p.QueryRow(ctx, `SELECT `+versionCols+` FROM skill_version v WHERE v.id = $1`, id))
	if err != nil {
		return skillstore.Version{}, fmt.Errorf("db: get skill version %d: %w", id, err)
	}
	return v, nil
}

// GetSkill implements skillstore.Store.
func (s *SkillStore) GetSkill(ctx context.Context, name string) (skillstore.Skill, error) {
	var sk skillstore.Skill
	err := s.p.QueryRow(ctx, `SELECT name, kind, pinned, position FROM skill WHERE name = $1`, name).
		Scan(&sk.Name, &sk.Kind, &sk.Pinned, &sk.Position)
	if err != nil {
		return skillstore.Skill{}, fmt.Errorf("db: get skill %s: %w", name, err)
	}
	return sk, nil
}

// ProductionVersion implements skillstore.Store.
func (s *SkillStore) ProductionVersion(ctx context.Context, skillName string) (skillstore.Version, bool, error) {
	v, err := scanVersion(s.p.QueryRow(ctx,
		`SELECT `+versionCols+` FROM skill_version v WHERE v.skill_name = $1 AND v.status = 'production'`, skillName))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return skillstore.Version{}, false, nil
		}
		return skillstore.Version{}, false, fmt.Errorf("db: production version of %s: %w", skillName, err)
	}
	return v, true, nil
}

// UpdateVersion implements skillstore.Store (status/rationale/ledger_seq/status_changed_at only — body
// and predicate are immutable once created; a rework is a new draft).
func (s *SkillStore) UpdateVersion(ctx context.Context, v skillstore.Version) error {
	tag, err := s.p.Exec(ctx, `
		UPDATE skill_version SET status = $2, rationale = $3, ledger_seq = $4, status_changed_at = $5
		WHERE id = $1`,
		v.ID, v.Status, v.Rationale, nullableSeq(v.LedgerSeq), v.StatusChangedAt)
	if err != nil {
		// A unique-violation here is the one-production partial index refusing a concurrent double
		// graduation — surface it as the invariant, not a raw constraint error.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("db: %s already has a production version (a concurrent graduation won): %w",
				v.SkillName, skillstore.ErrProductionExists)
		}
		return fmt.Errorf("db: update skill version %d: %w", v.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return skillstore.ErrNotFound
	}
	return nil
}

// ProductionSet lists every production row ordered by the skill's compose position (the composer's
// per-session snapshot read).
func (s *SkillStore) ProductionSet(ctx context.Context) ([]skillstore.Version, error) {
	rows, err := s.p.Query(ctx, `
		SELECT `+versionCols+` FROM skill_version v
		JOIN skill k ON k.name = v.skill_name
		WHERE v.status = 'production'
		ORDER BY k.position ASC`)
	if err != nil {
		return nil, fmt.Errorf("db: production set: %w", err)
	}
	defer rows.Close()
	var out []skillstore.Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func nullableSeq(seq int64) any {
	if seq == 0 {
		return nil
	}
	return seq
}
