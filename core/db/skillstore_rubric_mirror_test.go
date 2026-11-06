package db

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// TG-474 — the rubric MIRROR through the REAL pgx store: the row lands class=rubric + pinned, its content
// hash equals the embedded rubric's (the drift-check identity the worker logs on), and the write path
// refuses a draft against it with the CLASS sentinel. Killing mutation: point the import at a tampered
// body — the hash identity below reddens (executed at review).
func TestRubricMirrorThroughTheRealStore(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the rubric-mirror round-trip")
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
	defer func() {
		_, _ = p.Exec(ctx, `DELETE FROM skill_version WHERE skill_name = 'judge-rubric'`)
		_, _ = p.Exec(ctx, `DELETE FROM skill WHERE name = 'judge-rubric'`)
	}()

	st := NewSkillStore(p)
	chainTestInit(t, ctx, st, p)
	if err := st.PutSkill(ctx, skillstore.Skill{Name: "judge-rubric", Kind: "catalog", Pinned: true, Position: 1000, Class: skillstore.ClassRubric}); err != nil {
		t.Fatalf("put skill: %v", err)
	}
	rv, rbody := judge.RubricVersion(), string(judge.RubricJSON())
	if err := st.ImportCompiledVersion(ctx, "judge-rubric", rv, rbody, skillstore.AppliesWhen{}); err != nil {
		t.Fatalf("import: %v", err)
	}

	cur, ok, err := st.ProductionVersion(ctx, "judge-rubric")
	if err != nil || !ok {
		t.Fatalf("production version: %v ok=%v", err, ok)
	}
	if cur.Version != rv {
		t.Errorf("mirror version = %s, want the embedded %s", cur.Version, rv)
	}
	// THE DRIFT IDENTITY: stored hash == hash(embedded bytes). This is the exact comparison the worker's
	// boot drift-check logs on; red here = the mirror can lie about what judges sessions.
	if cur.ContentHash != skillstore.ContentHash(rbody, skillstore.AppliesWhen{}) {
		t.Errorf("mirror content hash does not match the embedded rubric — the projection lies")
	}
	// The row states its class + pin on the read surface.
	rows, err := st.ProductionRows(ctx)
	if err != nil {
		t.Fatalf("production rows: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.SkillName == "judge-rubric" {
			found = true
			if r.Class != skillstore.ClassRubric || !r.Pinned {
				t.Errorf("mirror row class=%q pinned=%v, want rubric/pinned", r.Class, r.Pinned)
			}
		}
	}
	if !found {
		t.Fatal("mirror row missing from ProductionRows")
	}
	// The write path refuses a draft with the CLASS law, through the real store.
	_, err = st.CreateVersion(ctx, skillstore.Version{SkillName: "judge-rubric", Version: "tamper", Body: "x", Rationale: "r"})
	if !errors.Is(err, skillstore.ErrRubricNeverDrafts) {
		t.Fatalf("draft against the mirror: got %v, want ErrRubricNeverDrafts", err)
	}
}
