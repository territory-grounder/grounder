package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/territory-grounder/grounder/core/skillstore"
)

// TestMigration0089PreservesLiveTrial is the 0089 prose-artifact-class oracle (spec/014
// REQ-1315/1316, ADR-0017, TG-470) on the real migrated schema. A FRESH-migrate DSN self-migrates to
// HEAD before any seeding, so "apply 0089 under a pre-existing trial" cannot literally be staged
// here; the honest oracle instead proves every property the metadata-only claim rests on:
//
//	(a) rows seeded WITHOUT a class default to 'skill' (the column default = the code default), a
//	    stated class round-trips, and the seeded production body reads back byte-identical;
//	(b) the one-active-trial partial unique index still refuses a second active trial — the live
//	    flywheel invariant 0089 must not loosen — with the trial's assignment intact;
//	(c) an INSERT with artifact_class='bogus' is refused by the schema CHECK (the closed vocabulary's
//	    schema half; the domain half is ErrUnknownClass, proven in core/skillstore);
//	(d) the widened skill_version_body_check admits 32768 bytes and refuses 32769 on a DIRECT insert
//	    around the domain gate (the schema ceiling is exactly the largest class's cap);
//	(e) the REQ-1316 pair: an 8193-byte body for class 'skill' is refused by the DOMAIN write path
//	    (the schema now admits it — only MaxBodyBytes stands between a skill and a runbook-sized
//	    body) while the SAME body for class 'runbook' is admitted.
//
// Gated on TG_TEST_POSTGRES_DSN (CI has no Postgres, constraint D5).
func TestMigration0089PreservesLiveTrial(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the 0089 class-model oracle")
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
	chainTestInit(t, ctx, s, p)

	base := fmt.Sprintf("m89-%d", os.Getpid())
	skillName, runbookName, bogusName := base+"-skill", base+"-runbook", base+"-bogus"
	defer func() {
		for _, n := range []string{skillName, runbookName, bogusName} {
			_, _ = p.Exec(ctx, "DELETE FROM skill_trial_assignment WHERE trial_id IN (SELECT id FROM skill_trial WHERE skill_name = $1)", n)
			_, _ = p.Exec(ctx, "DELETE FROM skill_trial WHERE skill_name = $1", n)
			_, _ = p.Exec(ctx, "DELETE FROM skill_version WHERE skill_name = $1", n)
			_, _ = p.Exec(ctx, "DELETE FROM skill WHERE name = $1", n)
		}
	}()

	// ---- (a) class defaults + round-trip + byte-identical body ----
	if err := s.PutSkill(ctx, skillstore.Skill{Name: skillName, Kind: "behavioral", Position: 90}); err != nil {
		t.Fatalf("put skill (no class): %v", err)
	}
	got, err := s.GetSkill(ctx, skillName)
	if err != nil {
		t.Fatal(err)
	}
	if got.Class != skillstore.ClassSkill {
		t.Fatalf("(a) a class-less PutSkill must read back as 'skill', got %q", got.Class)
	}
	if err := s.PutSkill(ctx, skillstore.Skill{Name: runbookName, Kind: "catalog", Class: skillstore.ClassRunbook, Position: 91}); err != nil {
		t.Fatalf("put runbook skill: %v", err)
	}
	if got, err = s.GetSkill(ctx, runbookName); err != nil || got.Class != skillstore.ClassRunbook {
		t.Fatalf("(a) a stated class must round-trip, got %q err=%v", got.Class, err)
	}
	prodBody := "production body under an active trial — must survive byte-for-byte"
	aw := skillstore.AppliesWhen{}
	mkVersion := func(name, ver, body string) skillstore.Version {
		v, err := s.CreateVersion(ctx, skillstore.Version{
			SkillName: name, Version: ver, Body: body, AppliesWhen: aw,
			ContentHash: skillstore.ContentHash(body, aw), Author: "m89", Source: "hand", Rationale: "0089 oracle",
		})
		if err != nil {
			t.Fatalf("create version %s v%s: %v", name, ver, err)
		}
		return v
	}
	prod := mkVersion(skillName, "1.0.0", prodBody)
	cand := mkVersion(skillName, "1.0.1", "candidate body")
	if back, err := s.GetVersion(ctx, prod.ID); err != nil || back.Body != prodBody {
		t.Fatalf("(a) the seeded body must read back byte-identical, err=%v", err)
	}
	// The default lands in the ROW, not just the Go struct: read the column raw.
	var rawClass string
	if err := p.QueryRow(ctx, "SELECT artifact_class FROM skill WHERE name = $1", skillName).Scan(&rawClass); err != nil || rawClass != "skill" {
		t.Fatalf("(a) raw artifact_class must be 'skill', got %q err=%v", rawClass, err)
	}

	// ---- (b) one-active-trial partial unique index + assignment intact ----
	trial, err := s.CreateTrial(ctx, skillstore.Trial{
		SkillName: skillName, CandidateIDs: []int64{cand.ID}, ControlVersionID: prod.ID,
		Dimension: "correct_diagnosis", MinSamplesPerArm: 15, MinLift: 0.05, PThreshold: 0.1,
		EndsAt: time.Now().Add(30 * 24 * time.Hour).UTC(), Note: "0089 oracle trial",
	})
	if err != nil {
		t.Fatalf("(b) create trial: %v", err)
	}
	if _, err := s.Assign(ctx, base+"-ref", trial.ID, 0); err != nil {
		t.Fatalf("(b) assign: %v", err)
	}
	_, err = s.CreateTrial(ctx, skillstore.Trial{
		SkillName: skillName, CandidateIDs: []int64{cand.ID}, ControlVersionID: prod.ID,
		Dimension: "correct_diagnosis", MinSamplesPerArm: 15, MinLift: 0.05, PThreshold: 0.1,
		EndsAt: time.Now().Add(30 * 24 * time.Hour).UTC(), Note: "must be refused",
	})
	if !isPgErr(err, "23505", "skill_trial_one_active") {
		t.Fatalf("(b) a second active trial must be refused by skill_trial_one_active, got %v", err)
	}
	if tr, ok, err := s.ActiveTrialFor(ctx, skillName); err != nil || !ok || tr.ID != trial.ID {
		t.Fatalf("(b) the FIRST trial must still be the active one, got %+v ok=%v err=%v", tr, ok, err)
	}
	var assigned int
	if err := p.QueryRow(ctx, "SELECT COUNT(*) FROM skill_trial_assignment WHERE trial_id = $1", trial.ID).Scan(&assigned); err != nil || assigned != 1 {
		t.Fatalf("(b) the live trial's assignment must be intact, got %d err=%v", assigned, err)
	}

	// ---- (c) the schema CHECK refuses an unknown class on a direct insert ----
	_, err = p.Exec(ctx, "INSERT INTO skill (name, kind, position, artifact_class) VALUES ($1, 'behavioral', 92, 'bogus')", bogusName)
	if !isPgErr(err, "23514", "artifact_class") {
		t.Fatalf("(c) artifact_class='bogus' must violate the CHECK, got %v", err)
	}

	// ---- (d) the widened body ceiling: 32768 admitted, 32769 refused, both AROUND the domain ----
	directInsert := func(ver string, n int) error {
		_, err := p.Exec(ctx, `
			INSERT INTO skill_version (skill_name, version, status, body, applies_when, content_hash, author, source, rationale, schema_version)
			VALUES ($1, $2, 'draft', $3, '{}', 'raw', 'm89', 'raw-sql', '0089 ceiling probe', 1)`,
			runbookName, ver, strings.Repeat("z", n))
		return err
	}
	if err := directInsert("9.0.0", 32768); err != nil {
		t.Fatalf("(d) 32768 bytes must be admitted by the widened schema ceiling, got %v", err)
	}
	if err := directInsert("9.0.1", 32769); !isPgErr(err, "23514", "skill_version_body_check") {
		t.Fatalf("(d) 32769 bytes must violate skill_version_body_check, got %v", err)
	}

	// ---- (e) the REQ-1316 pair oracle through the REAL pgx write path ----
	over8k := strings.Repeat("q", 8193)
	_, err = s.CreateVersion(ctx, skillstore.Version{
		SkillName: skillName, Version: "2.0.0", Body: over8k, AppliesWhen: aw,
		ContentHash: skillstore.ContentHash(over8k, aw), Author: "m89", Source: "hand", Rationale: "0089 pair oracle",
	})
	if !errors.Is(err, skillstore.ErrBodyBounds) {
		t.Fatalf("(e) 8193 bytes for class 'skill' must be refused by the DOMAIN cap, got %v", err)
	}
	if _, err = s.CreateVersion(ctx, skillstore.Version{
		SkillName: runbookName, Version: "2.0.0", Body: over8k, AppliesWhen: aw,
		ContentHash: skillstore.ContentHash(over8k, aw), Author: "m89", Source: "hand", Rationale: "0089 pair oracle",
	}); err != nil {
		t.Fatalf("(e) the SAME 8193 bytes for class 'runbook' must be admitted, got %v", err)
	}
}

// isPgErr reports whether err is the given Postgres error code and mentions marker (a constraint or
// column name) — so the oracle asserts WHICH invariant refused, not merely that something failed.
func isPgErr(err error, code, marker string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == code && (strings.Contains(pgErr.ConstraintName, marker) || strings.Contains(pgErr.Message, marker))
}
