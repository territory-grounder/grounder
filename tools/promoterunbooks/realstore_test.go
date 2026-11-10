package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// The pgx half of the graduation-rail proof: the REAL store, the REAL governance-ledger sink, and the
// TG-489 chain — which a promotion must move NOT AT ALL (transitions are chain-neutral: UpdateVersion
// touches only status/rationale/ledger_seq/status_changed_at, none of them ChainFacts). Same throwaway-
// database convention as the seeder's drills (the chain is global per-database state). Gated on
// TG_TEST_POSTGRES_DSN — the EMPTY-server fixture whose tests migrate for themselves.

// promoteDrillDB creates the throwaway database, migrates it, and returns a pool plus cleanup.
func promoteDrillDB(t *testing.T, ctx context.Context) *db.Pool {
	t.Helper()
	base := os.Getenv("TG_TEST_POSTGRES_DSN")
	if base == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the graduation-rail real-store drills")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	name := fmt.Sprintf("tg36_promote_%d", os.Getpid())
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

// seedFixture writes one identity + draft THROUGH the real store's governed writers (the chained
// CreateVersion), exactly as the seeder does.
func seedFixture(t *testing.T, ctx context.Context, s *db.SkillStore, name string, class skillstore.ArtifactClass, author string) skillstore.Version {
	t.Helper()
	kind := "catalog"
	if class == skillstore.ClassSkill {
		kind = "behavioral"
	}
	if err := s.PutSkill(ctx, skillstore.Skill{Name: name, Kind: kind, Class: class, Position: 500, Description: "drill artifact"}); err != nil {
		t.Fatalf("put %s: %v", name, err)
	}
	body := "runbook body for " + name
	v, err := s.CreateVersion(ctx, skillstore.Version{
		SkillName: name, Version: "1.0.0", Body: body,
		ContentHash: skillstore.ContentHash(body, skillstore.AppliesWhen{}),
		Author:      author, Source: "distill:src/" + name + ".md", Rationale: "[draft] drill fixture",
	})
	if err != nil {
		t.Fatalf("draft %s: %v", name, err)
	}
	return v
}

// promoteStore builds the tool's real-store adapter exactly as run() does: the pgx skill store plus a
// governance ledger continued from the persisted tail, write-through to governance_ledger.
func promoteStore(t *testing.T, ctx context.Context, p *db.Pool) dbStore {
	t.Helper()
	lstore := db.NewLedgerStore(p)
	seq, hash, err := lstore.Tail(ctx)
	if err != nil {
		t.Fatalf("ledger tail: %v", err)
	}
	return dbStore{s: db.NewSkillStore(p), lg: audit.NewLedgerFromTail(seq, hash).WithSink(lstore)}
}

// TestPromoteRealStoreEndToEnd proves the rail on the pgx path: seeded runbook drafts promote through
// skillstore.Transition (production re-readable — the wiki's resolve condition), a skill-class seeded
// draft and a foreign-authored runbook draft are untouched (one silent by the class filter, one a
// reported refusal), the governance ledger records exactly the promotions, the TG-489 chain head does
// not move (transitions are CHAIN-NEUTRAL — executed, not asserted), and a re-run promotes zero.
func TestPromoteRealStoreEndToEnd(t *testing.T) {
	ctx := context.Background()
	p := promoteDrillDB(t, ctx)
	s := db.NewSkillStore(p)
	if rep, err := s.EnsureChain(ctx); err != nil || !rep.OK {
		t.Fatalf("EnsureChain: rep=%+v err=%v", rep, err)
	}

	rbA := seedFixture(t, ctx, s, "rb-drill-alpha", skillstore.ClassRunbook, seedAuthor)
	rbB := seedFixture(t, ctx, s, "rb-drill-beta", skillstore.ClassRunbook, seedAuthor)
	sk := seedFixture(t, ctx, s, "sk-drill", skillstore.ClassSkill, seedAuthor)
	foreign := seedFixture(t, ctx, s, "rb-drill-foreign", skillstore.ClassRunbook, "operator:someone-else")

	head0, err := s.VerifyChainRead(ctx)
	if err != nil || !head0.OK || head0.Total != 4 {
		t.Fatalf("pre-promotion chain: rep=%+v err=%v", head0, err)
	}
	lstore := db.NewLedgerStore(p)
	seq0, _, err := lstore.Tail(ctx)
	if err != nil {
		t.Fatal(err)
	}

	st := promoteStore(t, ctx, p)
	plan, err := buildPlan(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if c := plan.counts(); len(plan.Items) != 3 || c.Promoted != 2 || c.Refused != 1 {
		t.Fatalf("plan must be 2 promote + 1 refuse (the skill identity never listed): %+v / %+v", c, plan.Items)
	}
	res := executePlan(ctx, plan, st)
	if res.Counts.Promoted != 2 || res.Counts.Refused != 1 || res.Counts.Skipped != 0 {
		t.Fatalf("execute: %+v", res.Counts)
	}
	if err := verifyPromoted(ctx, st, res.Promoted); err != nil {
		t.Fatalf("post-run verification: %v", err)
	}

	// The wiki resolve condition, read from the store directly: a runbook-class identity whose
	// production version is EXACTLY the promoted row (wikiRunbookPageHandler serves that row's body).
	for name, want := range map[string]skillstore.Version{"rb-drill-alpha": rbA, "rb-drill-beta": rbB} {
		got, ok, err := s.ProductionVersion(ctx, name)
		if err != nil || !ok || got.ID != want.ID {
			t.Fatalf("%s must resolve as the promoted production row (id %d): got ok=%v id=%d err=%v",
				name, want.ID, ok, got.ID, err)
		}
		if !strings.Contains(got.Rationale, "TG-488") || !strings.Contains(got.Rationale, "/v1/wiki/runbook/"+name) {
			t.Errorf("%s rationale must cite the ruling and the wiki destination: %s", name, got.Rationale)
		}
		if got.LedgerSeq == 0 {
			t.Errorf("%s: promotion must be ledger-recorded", name)
		}
	}
	// Untouched rows: the skill-class seeded draft (class filter) and the foreign runbook draft.
	for name, id := range map[string]int64{"sk-drill": sk.ID, "rb-drill-foreign": foreign.ID} {
		v, err := s.GetVersion(ctx, id)
		if err != nil || v.Status != skillstore.StatusDraft {
			t.Fatalf("%s must remain an untouched draft: status=%s err=%v", name, v.Status, err)
		}
	}

	// CHAIN NEUTRALITY, executed: the promotion moved the TG-489 chain not at all.
	rep, err := s.VerifyChainRead(ctx)
	if err != nil || !rep.OK || rep.Head != head0.Head || rep.Total != head0.Total {
		t.Fatalf("transitions must be chain-neutral (head unchanged): before=%+v after=%+v err=%v", head0, rep, err)
	}
	// The governance ledger recorded EXACTLY the two promotions (no incumbents, so no retires).
	seq1, _, err := lstore.Tail(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if seq1 != seq0+2 {
		t.Fatalf("governance ledger must advance by exactly 2 (one per promotion): %d -> %d", seq0, seq1)
	}

	// Idempotency on the real store: a fresh run (fresh tail-continued ledger) promotes ZERO; the
	// standing foreign draft still refuses; neither the chain nor the ledger moves.
	st2 := promoteStore(t, ctx, p)
	plan2, err := buildPlan(ctx, st2)
	if err != nil {
		t.Fatal(err)
	}
	res2 := executePlan(ctx, plan2, st2)
	if res2.Counts.Promoted != 0 || res2.Counts.Skipped != 2 || res2.Counts.Refused != 1 {
		t.Fatalf("re-run must promote ZERO (skip the two pages, still refuse the foreign draft): %+v", res2.Counts)
	}
	rep2, err := s.VerifyChainRead(ctx)
	if err != nil || !rep2.OK || rep2.Head != head0.Head {
		t.Fatalf("re-run must not move the chain: rep=%+v err=%v", rep2, err)
	}
	seq2, _, err := lstore.Tail(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if seq2 != seq1 {
		t.Fatalf("re-run must not append to the governance ledger: %d -> %d", seq1, seq2)
	}
}
