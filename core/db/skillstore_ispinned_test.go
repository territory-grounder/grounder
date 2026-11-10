package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/skillstore"
)

// TG-218. StartTrial's pin refusal is only as good as the pin lookup underneath it, and that lookup is
// SQL — the one part of this change no in-memory oracle can execute. A wrong column, a wrong table or a
// mishandled no-rows case all produce `false`, which is precisely the answer that lets a trial open on
// the floor.
//
// The no-rows case is asserted deliberately: `QueryRow(...).Scan(...)` on a missing skill returns
// pgx.ErrNoRows, and swallowing that as a bare `false, err` would make StartTrial fail closed on a
// skill that simply has not been imported yet — while returning `true` on error would refuse every
// trial in the system the moment the query broke. Neither is what we want, so both are pinned here.
func TestIsPinnedReadsTheRealColumn(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the IsPinned round-trip test")
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

	name := fmt.Sprintf("rt-pin-skill-%d", os.Getpid())
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM skill_trial WHERE skill_name = $1", name)
		_, _ = p.Exec(ctx, "DELETE FROM skill_version WHERE skill_name = $1", name)
		_, _ = p.Exec(ctx, "DELETE FROM skill WHERE name = $1", name)
	}()

	// Unknown skill — not pinned, and NOT an error.
	switch pinned, err := s.IsPinned(ctx, name+"-does-not-exist"); {
	case err != nil:
		t.Errorf("IsPinned errored on an unknown skill: %v. StartTrial turns any error into a refusal, so "+
			"this would block trials on every skill not yet imported.", err)
	case pinned:
		t.Error("IsPinned reported an unknown skill as PINNED — that refuses trials that were never on the floor")
	}

	if err := s.PutSkill(ctx, skillstore.Skill{Name: name, Kind: "behavioral", Position: 97}); err != nil {
		t.Fatalf("put skill: %v", err)
	}
	if pinned, err := s.IsPinned(ctx, name); err != nil || pinned {
		t.Fatalf("a freshly created skill reads pinned=%v (err %v) — the unpinned case must read false or "+
			"the pinned case below proves nothing", pinned, err)
	}

	if err := s.PutSkill(ctx, skillstore.Skill{Name: name, Kind: "behavioral", Pinned: true, Position: 97}); err != nil {
		t.Fatalf("pin skill: %v", err)
	}
	pinned, err := s.IsPinned(ctx, name)
	if err != nil {
		t.Fatalf("IsPinned after pinning: %v", err)
	}
	if !pinned {
		t.Fatal("IsPinned returned false for a skill whose `pinned` column is true — StartTrial's refusal " +
			"is then unreachable and the floor stays experimentable")
	}

	// The refusal end to end, over the REAL store.
	_, err = skillstore.StartTrial(ctx, s, skillstore.Trial{
		SkillName: name, CandidateIDs: []int64{1}, Dimension: "appropriate_band",
		MinSamplesPerArm: 5, EndsAt: time.Now().Add(72 * time.Hour),
	}, time.Now())
	if !errors.Is(err, skillstore.ErrPinnedSkill) {
		t.Errorf("StartTrial over the pgx store returned %v, want ErrPinnedSkill — the refusal has to hold "+
			"on the store production actually uses, not only on MemTrialStore", err)
	}
}
