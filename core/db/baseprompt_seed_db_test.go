package db

// C-3b fix-forward — the base-prompt seed row against the REAL schema (openFixture / TG_TEST_DSN). The
// first deploy's seed failed live on skill_kind_check (Kind "prompt" ∉ {behavioral, catalog}, SQLSTATE
// 23514) — the embed fallback held, but the row never seeded, and no local drill exercised the exact
// (Kind, Class) pair the worker writes. This drill seeds THAT row; a schema-vocabulary drift (or another
// invented Kind) now fails in CI, not on the prod boot log.

import (
	"fmt"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/skillstore"
)

func TestBasePromptSeedRowSatisfiesTheSchema(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	st := NewSkillStore(p)
	// NO DELETE CLEANUP — skill_version is CHAIN-LINKED (TG-489): deleting a row is indistinguishable
	// from tampering and permanently breaks VerifyChainRead for every later reader (measured: this
	// drill's first form deleted its row and the whole package's ProductionRows reads went red with
	// "chain BROKEN … link-mismatch" on the next run). Uniquely-named rows are simply LEFT — accreting
	// append-only rows is the chained table's design, and the name prefix keeps the real seed name clean.
	name := fmt.Sprintf("tg497test-base-prompt-guidance-%d", time.Now().UnixNano())
	if _, err := st.EnsureChain(ctx); err != nil { // the TG-489 chain precondition worker boot establishes
		t.Fatalf("EnsureChain: %v", err)
	}

	// The EXACT shape cmd/worker's importCompiledSkills writes for the guidance row (unique name so
	// repeated and parallel gate runs never collide and never need a chain-tampering delete).
	if err := st.PutSkill(ctx, skillstore.Skill{
		Name: name, Kind: "behavioral", Pinned: false, Position: 999,
		Class: skillstore.ClassPrompt,
	}); err != nil {
		t.Fatalf("the seeder's identity row must satisfy the live schema (kind CHECK included): %v", err)
	}
	if err := st.ImportCompiledVersion(ctx, name, "1.0.0",
		"guidance body", skillstore.AppliesWhen{}); err != nil {
		t.Fatalf("the seeder's version import must succeed: %v", err)
	}
	rows, err := st.ProductionRows(ctx)
	if err != nil {
		t.Fatalf("ProductionRows: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.SkillName == name {
			found = true
			if skillstore.DefaultClass(r.Class) != skillstore.ClassPrompt {
				t.Fatalf("the production row must carry ClassPrompt (the compose path keys on it), got %q", r.Class)
			}
		}
	}
	if !found {
		t.Fatal("the seeded prompt row must surface in the composer's production snapshot")
	}

	// The vocabulary pin: the Kind the first deploy tried MUST still be refused — if someone widens the
	// CHECK, this drill says so and the comment above gets rewritten deliberately, not silently.
	if err := st.PutSkill(ctx, skillstore.Skill{
		Name: name, Kind: "prompt", Class: skillstore.ClassPrompt,
	}); err == nil {
		t.Fatal("Kind \"prompt\" unexpectedly ADMITTED — the 0009 kind vocabulary widened; revisit the seeder's kind choice deliberately")
	}
}
