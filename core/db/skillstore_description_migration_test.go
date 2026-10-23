package db

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/skillstore"
)

// TestMigration0090DescriptionSemantics is the 0090 oracle (TG-55/TG-476, ADR-0012) on the real
// migrated schema: the canonical description column exists with the honest default, round-trips
// through the pgx store, and — the semantic that matters operationally — SURVIVES the boot importer's
// idempotent empty-description re-upsert. Without the PutSkill CASE guard, every worker restart would
// blank every operator-written description; (b) below reddens if that guard is dropped (killing
// mutation executed during development). Gated on TG_TEST_POSTGRES_DSN (CI has no Postgres, D5).
func TestMigration0090DescriptionSemantics(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the 0090 description oracle")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()
	s := NewSkillStore(p)

	name := fmt.Sprintf("m90-%d-runbook", os.Getpid())
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM skill WHERE name = $1", name)
	}()

	// (a) A stated description round-trips through PutSkill/GetSkill and both console read queries.
	want := "How to drain and verify a full guest disk."
	if err := s.PutSkill(ctx, skillstore.Skill{
		Name: name, Kind: "catalog", Position: 40, Class: skillstore.ClassRunbook, Description: want,
	}); err != nil {
		t.Fatalf("put with description: %v", err)
	}
	sk, err := s.GetSkill(ctx, name)
	if err != nil || sk.Description != want {
		t.Fatalf("GetSkill description: got %q err=%v, want %q", sk.Description, err, want)
	}
	if det, ok, derr := s.SkillDetail(ctx, name); derr != nil || !ok || det.Description != want {
		t.Fatalf("SkillDetail description: got %+v ok=%v err=%v", det.Description, ok, derr)
	}
	list, err := s.ListSkills(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, r := range list {
		if r.Name == name {
			found = true
			if r.Description != want {
				t.Fatalf("ListSkills description: got %q, want %q", r.Description, want)
			}
		}
	}
	if !found {
		t.Fatal("seeded skill missing from ListSkills")
	}

	// (b) The boot importer's shape — an idempotent re-upsert with the EMPTY description — must
	// preserve the stored text, not blank it.
	if err := s.PutSkill(ctx, skillstore.Skill{
		Name: name, Kind: "catalog", Position: 40, Class: skillstore.ClassRunbook,
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if sk, err = s.GetSkill(ctx, name); err != nil || sk.Description != want {
		t.Fatalf("description after empty re-upsert: got %q err=%v, want preserved %q", sk.Description, err, want)
	}

	// (c) A NEW non-empty description replaces the old — the preserve rule is empty-only, not sticky.
	if err := s.PutSkill(ctx, skillstore.Skill{
		Name: name, Kind: "catalog", Position: 40, Class: skillstore.ClassRunbook, Description: "Revised.",
	}); err != nil {
		t.Fatalf("replace description: %v", err)
	}
	if sk, err = s.GetSkill(ctx, name); err != nil || sk.Description != "Revised." {
		t.Fatalf("description after replace: got %q err=%v", sk.Description, err)
	}

	// (d) The column default is the honest empty string — a row inserted around the store (omitting
	// the column, the pre-0090 writer's shape) reads back "".
	raw := name + "-raw"
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM skill WHERE name = $1", raw) }()
	if _, err := p.Exec(ctx, "INSERT INTO skill (name, kind, pinned, position) VALUES ($1, 'catalog', false, 41)", raw); err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	if sk, err = s.GetSkill(ctx, raw); err != nil || sk.Description != "" {
		t.Fatalf("default description: got %q err=%v, want empty", sk.Description, err)
	}
}
