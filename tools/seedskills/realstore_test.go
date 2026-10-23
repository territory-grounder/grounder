package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// The pgx half of the seeding-rail proof: the REAL store write path, which since TG-489 is the
// CHAIN-LINKED writer (chain participation is internal to db.SkillStore.CreateVersion — head
// locked FOR UPDATE, link computed, head advanced, one transaction). These drills run against a
// DEDICATED throwaway database created for this process (the chain is global per-database
// state; only true isolation gives an honest genesis), the same convention as core/db's
// TG-489 chain drills. Gated on TG_TEST_POSTGRES_DSN — the EMPTY-server fixture whose tests
// migrate for themselves.

// seedDrillDB creates the throwaway database on TG_TEST_POSTGRES_DSN's server, migrates it,
// and returns a pool plus cleanup. Skips when the DSN is unset, like every DSN-gated test.
func seedDrillDB(t *testing.T, ctx context.Context) *db.Pool {
	t.Helper()
	base := os.Getenv("TG_TEST_POSTGRES_DSN")
	if base == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the seeding-rail real-store drills")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	name := fmt.Sprintf("tg36_seed_%d", os.Getpid())
	admin, err := db.Connect(ctx, base)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name) // a crashed prior run's leftover
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatalf("create drill database: %v", err)
	}
	u.Path = "/" + name
	dsn := u.String()
	if err := db.Migrate(ctx, dsn); err != nil {
		admin.Close()
		t.Fatalf("migrate drill database: %v", err)
	}
	p, err := db.Connect(ctx, dsn)
	if err != nil {
		admin.Close()
		t.Fatalf("drill pool: %v", err)
	}
	t.Cleanup(func() {
		p.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close()
	})
	return p
}

// TestSeedRealStoreChainVerifiesAndRerunIsNoOp is the rail's end-to-end proof on the pgx path:
// EnsureChain (the worker-boot mirror) → seed the REAL committed corpus → the TG-489 chain
// verifies over every appended row → a re-run creates nothing and moves the chain not at all —
// and the drafts are inert (zero production rows compose).
func TestSeedRealStoreChainVerifiesAndRerunIsNoOp(t *testing.T) {
	ctx := context.Background()
	p := seedDrillDB(t, ctx)
	s := db.NewSkillStore(p)

	// Pre-chain, the chained writer fails CLOSED: a FULLY VALID draft (identity present,
	// rationale stated, hash bound) is still refused until EnsureChain has run — the seeder
	// cannot write around an uninitialized chain even if it tried.
	if err := s.PutSkill(ctx, skillstore.Skill{Name: "prechain-probe", Kind: "behavioral", Class: skillstore.ClassSkill, Position: 998}); err != nil {
		t.Fatalf("put probe identity: %v", err)
	}
	_, err := s.CreateVersion(ctx, skillstore.Version{
		SkillName: "prechain-probe", Version: "0.0.1", Body: "probe",
		ContentHash: skillstore.ContentHash("probe", skillstore.AppliesWhen{}),
		Author:      seedAuthor, Source: "distill:src/probe.md", Rationale: "[draft] pre-chain probe",
	})
	if !errors.Is(err, db.ErrChainUninitialized) {
		t.Fatalf("a valid draft against an uninitialized chain must refuse with ErrChainUninitialized, got: %v", err)
	}
	rep, err := s.EnsureChain(ctx)
	if err != nil || !rep.OK {
		t.Fatalf("EnsureChain on a fresh store: rep=%+v err=%v", rep, err)
	}

	arts, err := loadCorpus(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	st := dbStore{s}

	plan1, err := buildPlan(ctx, arts, st)
	if err != nil {
		t.Fatal(err)
	}
	c1, err := apply(ctx, plan1, st)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if c1.Created != 48 || c1.Skipped != 0 {
		t.Fatalf("first run must create all 48 drafts: %+v", c1)
	}

	// The chain still VERIFIES after seeding: every appended row is linked (a row written
	// around the chained writer would surface as missing-link) and the head advanced 48 times.
	rep, err = s.VerifyChainRead(ctx)
	if err != nil || !rep.OK || rep.Total != 48 || rep.Verified != 48 {
		t.Fatalf("post-seed chain must verify 48 of 48: rep=%+v err=%v", rep, err)
	}

	// Idempotency on the real store: the re-run creates nothing and the chain does not move.
	plan2, err := buildPlan(ctx, arts, st)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := apply(ctx, plan2, st)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if c2.Created != 0 || c2.Skipped != 48 {
		t.Fatalf("re-run must skip all 48: %+v", c2)
	}
	rep2, err := s.VerifyChainRead(ctx)
	if err != nil || !rep2.OK || rep2.Total != 48 || rep2.Head != rep.Head {
		t.Fatalf("re-run must not move the chain: rep=%+v err=%v", rep2, err)
	}

	// One row spot-checked end-to-end through the console read: draft status, tool author,
	// distill provenance, class + description on the identity.
	det, found, err := s.SkillDetail(ctx, "alert-queue-review")
	if err != nil || !found {
		t.Fatalf("seeded skill must be readable: found=%v err=%v", found, err)
	}
	if det.ArtifactClass != string(skillstore.ClassSkill) || det.Kind != "behavioral" || det.Description == "" {
		t.Errorf("identity contract broken: %+v", det.SkillSummary)
	}
	if len(det.Versions) != 1 || det.Versions[0].Status != string(skillstore.StatusDraft) ||
		det.Versions[0].Author != seedAuthor || !strings.HasPrefix(det.Versions[0].Source, "distill:") {
		t.Errorf("draft row contract broken: %+v", det.Versions)
	}

	// Drafts are INERT: nothing seeded reaches the composer's snapshot.
	rows, err := s.ProductionRows(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("44 drafts must compose ZERO production rows: rows=%d err=%v", len(rows), err)
	}
}

// TestRealStoreSurfacesItsOwnBodyCap executes the oversize refusal against the REAL write gate:
// the per-class cap is the store's law (skillstore.MaxBodyBytes via ValidateDraft), and the
// seeder's plan-time cap is the SAME law — so what loadCorpus refuses offline, CreateVersion
// refuses here with the same named error.
func TestRealStoreSurfacesItsOwnBodyCap(t *testing.T) {
	ctx := context.Background()
	p := seedDrillDB(t, ctx)
	s := db.NewSkillStore(p)
	if _, err := s.EnsureChain(ctx); err != nil {
		t.Fatalf("ensure chain: %v", err)
	}
	if err := s.PutSkill(ctx, skillstore.Skill{Name: "oversize-drill", Kind: "behavioral", Class: skillstore.ClassSkill, Position: 999}); err != nil {
		t.Fatalf("put skill: %v", err)
	}
	body := strings.Repeat("x", skillstore.MaxBodyBytes(skillstore.ClassSkill)+1)
	_, err := s.CreateVersion(ctx, skillstore.Version{
		SkillName: "oversize-drill", Version: "0.1.0", Body: body,
		ContentHash: skillstore.ContentHash(body, skillstore.AppliesWhen{}),
		Author:      seedAuthor, Source: "distill:src/oversize.md", Rationale: "[draft] oversize drill",
	})
	if !errors.Is(err, skillstore.ErrBodyBounds) {
		t.Fatalf("the real store must refuse an oversize skill body with ErrBodyBounds, got: %v", err)
	}
}
