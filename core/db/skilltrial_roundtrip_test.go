package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/skillstore"
)

// TestSkillTrialRoundTrip drives the REAL pgx path for skill_trial — CreateTrial → ActiveTrialFor — so a
// dropped Welch parameter (MinLift, PThreshold, MinSamplesPerArm) fails HERE, not silently in prod where it
// would corrupt the finalizer's verdict. These are the load-bearing statistics of the flywheel's exceed-thesis
// (the trials that just started run on exactly these). Gated on TG_TEST_POSTGRES_DSN; part of the pgx
// round-trip coverage backfill the persistence audit recommended.
func TestSkillTrialRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the skill_trial round-trip test")
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

	name := fmt.Sprintf("rt-trial-skill-%d", os.Getpid())
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM skill_trial WHERE skill_name = $1", name)
		_, _ = p.Exec(ctx, "DELETE FROM skill_version WHERE skill_name = $1", name)
		_, _ = p.Exec(ctx, "DELETE FROM skill WHERE name = $1", name)
	}()

	if err := s.PutSkill(ctx, skillstore.Skill{Name: name, Kind: "behavioral", Position: 98}); err != nil {
		t.Fatalf("put skill: %v", err)
	}
	mkVersion := func(ver, body string) int64 {
		aw := skillstore.AppliesWhen{}
		v, err := s.CreateVersion(ctx, skillstore.Version{
			SkillName: name, Version: ver, Body: body, AppliesWhen: aw,
			ContentHash: skillstore.ContentHash(body, aw), Author: "rt", Source: "flywheel:discovery", Rationale: "rt",
		})
		if err != nil {
			t.Fatalf("create version %s: %v", ver, err)
		}
		return v.ID
	}
	controlID := mkVersion("1.0.0-ctl", "control body for the trial round-trip test")
	candID := mkVersion("1.0.0-cand", "candidate body for the trial round-trip test")

	endsAt := time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC)
	created, err := s.CreateTrial(ctx, skillstore.Trial{
		SkillName: name, CandidateIDs: []int64{candID}, ControlVersionID: controlID,
		Dimension: "falsifiable_prediction", MinSamplesPerArm: 15, MinLift: 0.2, PThreshold: 0.05,
		EndsAt: endsAt, Note: "round-trip guard",
	})
	if err != nil {
		t.Fatalf("create trial: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("create trial must return an id")
	}

	got, ok, err := s.ActiveTrialFor(ctx, name)
	if err != nil || !ok {
		t.Fatalf("active trial: ok=%v err=%v", ok, err)
	}
	// The load-bearing Welch parameters MUST round-trip exactly — a silent drop corrupts the verdict.
	if got.MinSamplesPerArm != 15 || got.MinLift != 0.2 || got.PThreshold != 0.05 {
		t.Fatalf("Welch params must round-trip: min_samples=%d min_lift=%v p=%v", got.MinSamplesPerArm, got.MinLift, got.PThreshold)
	}
	if got.Dimension != "falsifiable_prediction" || got.ControlVersionID != controlID ||
		len(got.CandidateIDs) != 1 || got.CandidateIDs[0] != candID {
		t.Fatalf("trial identity must round-trip: %+v (want control=%d cand=%d)", got, controlID, candID)
	}
	// EndsAt survives the timestamptz round-trip (compare the instant, robust to precision/zone).
	if !got.EndsAt.Equal(endsAt) {
		t.Fatalf("ends_at must round-trip: got %s want %s", got.EndsAt, endsAt)
	}
}
