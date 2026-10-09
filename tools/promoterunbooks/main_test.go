package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// The pure-Go half of the graduation-rail proof (constraint D5: acceptance runs without Postgres).
// The adapter drives the REAL skillstore.Transition state machine over the in-memory store and an
// in-memory governance ledger — the same function the pgx path binds — so the TG-476 runbook lane,
// the class refusals, and per-row isolation are executed here, not asserted by inspection. The pgx
// path (and chain neutrality) is proven in realstore_test.go.

// memPromoteStore adapts skillstore.MemStore to the promoter's Store surface and counts Transition
// calls, so dry-run-writes-nothing and re-run-writes-nothing are executed assertions.
type memPromoteStore struct {
	m           *skillstore.MemStore
	lg          *audit.Ledger
	ids         []Identity // MemStore has no identity enumeration; the fixture tracks its own puts
	transitions int
	failID      int64 // inject a store outage at this version's row UPDATE (mid-run failure drill)
}

func newMemPromoteStore() *memPromoteStore {
	return &memPromoteStore{m: skillstore.NewMemStore(), lg: audit.NewLedger()}
}

func (s *memPromoteStore) Skills(context.Context) ([]Identity, error) { return s.ids, nil }

func (s *memPromoteStore) Versions(_ context.Context, name string) ([]VersionRow, error) {
	var out []VersionRow
	for _, v := range s.m.VersionsOf(name) {
		out = append(out, VersionRow{ID: v.ID, Version: v.Version, Status: v.Status, Author: v.Author})
	}
	return out, nil
}

func (s *memPromoteStore) Transition(ctx context.Context, id int64, to skillstore.Status, rationale string) (skillstore.Version, error) {
	s.transitions++
	var st skillstore.Store = s.m
	if id == s.failID {
		st = outageStore{s.m}
	}
	return skillstore.Transition(ctx, st, s.lg, id, to, rationale)
}

// outageStore fails the row UPDATE mid-transition (after the state machine admitted the move) — the
// injected store outage the per-row-isolation drill needs.
type outageStore struct{ skillstore.Store }

func (o outageStore) UpdateVersion(context.Context, skillstore.Version) error {
	return errors.New("injected store outage")
}

func (s *memPromoteStore) identity(name string, class skillstore.ArtifactClass) {
	kind := "catalog"
	if class == skillstore.ClassSkill {
		kind = "behavioral"
	}
	s.m.PutSkill(skillstore.Skill{Name: name, Kind: kind, Class: class, Position: 500})
	s.ids = append(s.ids, Identity{Name: name, Class: class})
}

func (s *memPromoteStore) draft(t *testing.T, name, author, version string) skillstore.Version {
	t.Helper()
	body := "runbook body for " + name + " v" + version
	v, err := s.m.CreateVersion(context.Background(), skillstore.Version{
		SkillName: name, Version: version, Body: body,
		ContentHash: skillstore.ContentHash(body, skillstore.AppliesWhen{}),
		Author:      author, Source: "distill:src/" + name + ".md",
		Rationale: "[draft] test fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (s *memPromoteStore) statusOf(t *testing.T, id int64) skillstore.Status {
	t.Helper()
	v, err := s.m.GetVersion(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return v.Status
}

// runOnce is the tool's execute path over the fixture: plan → execute → verify.
func runOnce(t *testing.T, st *memPromoteStore) Result {
	t.Helper()
	ctx := context.Background()
	p, err := buildPlan(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	res := executePlan(ctx, p, st)
	if err := verifyPromoted(ctx, st, res.Promoted); err != nil {
		t.Fatalf("post-run verification: %v", err)
	}
	return res
}

// ---------------------------------------------------------------------------------------------------
// The class filter — red-proven load-bearing.
// ---------------------------------------------------------------------------------------------------

// TestClassFilterIsLoadBearing: a SKILL-class draft with the SAME seeded author and the SAME draft
// status is untouched — and the test first proves that on author+status alone the decision layer
// WOULD promote it, so the class filter is the ONLY thing standing (remove it and this test reds).
func TestClassFilterIsLoadBearing(t *testing.T) {
	ctx := context.Background()
	st := newMemPromoteStore()
	st.identity("incident-lifecycle", skillstore.ClassRunbook)
	rb := st.draft(t, "incident-lifecycle", seedAuthor, "1.0.0")
	st.identity("alert-queue-review", skillstore.ClassSkill)
	sk := st.draft(t, "alert-queue-review", seedAuthor, "1.0.0")

	// The red-prove: the skill-class rows pass every check EXCEPT the class. If decide would not
	// promote them, the fixture rotted and the exclusion below would prove nothing.
	skRows, err := st.Versions(ctx, "alert-queue-review")
	if err != nil {
		t.Fatal(err)
	}
	if it, listed := decide("alert-queue-review", skRows); !listed || it.Action != ActionPromote {
		t.Fatalf("fixture rot: on author+status alone the skill-class draft must be promotable (got %+v) — "+
			"otherwise its exclusion cannot be attributed to the class filter", it)
	}

	p, err := buildPlan(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 1 || p.Items[0].Name != "incident-lifecycle" {
		t.Fatalf("plan must carry ONLY the runbook (the class filter is load-bearing): %+v", p.Items)
	}
	res := executePlan(ctx, p, st)
	if res.Counts.Promoted != 1 || res.Counts.Refused != 0 {
		t.Fatalf("execute: %+v", res.Counts)
	}
	if got := st.statusOf(t, rb.ID); got != skillstore.StatusProduction {
		t.Fatalf("runbook draft must be production, got %s", got)
	}
	if got := st.statusOf(t, sk.ID); got != skillstore.StatusDraft {
		t.Fatalf("the SKILL-class draft must be UNTOUCHED (still draft), got %s — the class filter broke", got)
	}
}

// ---------------------------------------------------------------------------------------------------
// Idempotency + the promoted-row contract.
// ---------------------------------------------------------------------------------------------------

// TestRunTwiceIsIdempotent: run 1 promotes every seeded runbook draft through the real state machine;
// run 2 promotes ZERO (already-production rows skip) and performs zero Transition calls — proven by
// the executed counter, not by inspection. The promoted row carries the graduation contract: ledger
// seq recorded, rationale citing TG-36/TG-488 and the wiki destination.
func TestRunTwiceIsIdempotent(t *testing.T) {
	st := newMemPromoteStore()
	rows := map[string]skillstore.Version{}
	for _, name := range []string{"rb-alpha", "rb-beta", "rb-gamma"} {
		st.identity(name, skillstore.ClassRunbook)
		rows[name] = st.draft(t, name, seedAuthor, "1.0.0")
	}

	res := runOnce(t, st)
	if res.Counts.Promoted != 3 || res.Counts.Skipped != 0 || res.Counts.Refused != 0 {
		t.Fatalf("first run must promote all 3: %+v", res.Counts)
	}
	v, err := st.m.GetVersion(context.Background(), rows["rb-beta"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != skillstore.StatusProduction || v.LedgerSeq == 0 {
		t.Fatalf("promoted row must be production with a ledger seq: status=%s seq=%d", v.Status, v.LedgerSeq)
	}
	for _, cite := range []string{"[production]", "TG-36", "TG-488", "TG-476", promoteActor, "/v1/wiki/runbook/rb-beta"} {
		if !strings.Contains(v.Rationale, cite) {
			t.Errorf("promotion rationale misses %q: %s", cite, v.Rationale)
		}
	}

	before := st.transitions
	res2 := runOnce(t, st)
	if res2.Counts.Promoted != 0 || res2.Counts.Skipped != 3 || res2.Counts.Refused != 0 {
		t.Fatalf("re-run must promote ZERO and skip all 3: %+v", res2.Counts)
	}
	if st.transitions != before {
		t.Fatalf("re-run performed %d Transition calls — the rail is not idempotent", st.transitions-before)
	}
}

// TestDryRunPlanWritesNothing: the whole dry-run is buildPlan — zero Transition calls, statuses
// untouched, by executed count.
func TestDryRunPlanWritesNothing(t *testing.T) {
	st := newMemPromoteStore()
	st.identity("rb-alpha", skillstore.ClassRunbook)
	d := st.draft(t, "rb-alpha", seedAuthor, "1.0.0")
	p, err := buildPlan(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if c := p.counts(); c.Promoted != 1 {
		t.Fatalf("plan must carry the promote: %+v", c)
	}
	if st.transitions != 0 {
		t.Fatalf("planning performed %d Transition calls — a dry-run must write NOTHING", st.transitions)
	}
	if got := st.statusOf(t, d.ID); got != skillstore.StatusDraft {
		t.Fatalf("dry-run mutated a row to %s", got)
	}
}

// ---------------------------------------------------------------------------------------------------
// Refusal oracles — each executed red.
// ---------------------------------------------------------------------------------------------------

// TestForeignAuthorDraftRefuses: a runbook draft NOT authored by tool:seedskills is refused (reported,
// non-zero arithmetic) and left exactly as it was — someone else's draft is not ours to promote.
func TestForeignAuthorDraftRefuses(t *testing.T) {
	st := newMemPromoteStore()
	st.identity("rb-foreign", skillstore.ClassRunbook)
	d := st.draft(t, "rb-foreign", "operator:someone-else", "0.9.0")

	res := runOnce(t, st)
	if res.Counts.Refused != 1 || res.Counts.Promoted != 0 {
		t.Fatalf("foreign-authored draft must refuse: %+v", res.Counts)
	}
	if got := st.statusOf(t, d.ID); got != skillstore.StatusDraft {
		t.Fatalf("refused row must be untouched (draft), got %s", got)
	}
	p, err := buildPlan(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if p.Items[0].Action != ActionRefuse || !strings.Contains(p.Items[0].Reason, "not ours to promote") {
		t.Fatalf("the refusal must name the law: %+v", p.Items[0])
	}
}

// TestForeignProductionIncumbentRefuses: promoting our seeded draft would RETIRE another writer's live
// page (the incumbent supersede) — that is an operator's call, so the row refuses and both rows stand.
func TestForeignProductionIncumbentRefuses(t *testing.T) {
	ctx := context.Background()
	st := newMemPromoteStore()
	st.identity("rb-contested", skillstore.ClassRunbook)
	foreign := st.draft(t, "rb-contested", "operator:someone-else", "0.9.0")
	// The operator publishes THEIR draft through the real runbook lane — the legitimate fixture.
	if _, err := skillstore.Transition(ctx, st.m, st.lg, foreign.ID, skillstore.StatusProduction, "operator publish"); err != nil {
		t.Fatal(err)
	}
	ours := st.draft(t, "rb-contested", seedAuthor, "1.0.0")

	res := runOnce(t, st)
	if res.Counts.Refused != 1 || res.Counts.Promoted != 0 {
		t.Fatalf("a foreign production incumbent must refuse the promote: %+v", res.Counts)
	}
	if got := st.statusOf(t, foreign.ID); got != skillstore.StatusProduction {
		t.Fatalf("the foreign live page must STAY production, got %s", got)
	}
	if got := st.statusOf(t, ours.ID); got != skillstore.StatusDraft {
		t.Fatalf("our draft must stay draft, got %s", got)
	}
}

// TestTransitionFailureMidRunLeavesOthersPromoted: per-row isolation — a store outage on one row
// refuses THAT row, the other rows promote, and the arithmetic reports the refusal (the caller exits
// non-zero on it). Unlike the seeder's whole-run refusal: promotion is per-artifact graduation.
func TestTransitionFailureMidRunLeavesOthersPromoted(t *testing.T) {
	st := newMemPromoteStore()
	ids := map[string]int64{}
	for _, name := range []string{"rb-alpha", "rb-doomed", "rb-omega"} {
		st.identity(name, skillstore.ClassRunbook)
		ids[name] = st.draft(t, name, seedAuthor, "1.0.0").ID
	}
	st.failID = ids["rb-doomed"]

	ctx := context.Background()
	p, err := buildPlan(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	res := executePlan(ctx, p, st)
	if res.Counts.Promoted != 2 || res.Counts.Refused != 1 {
		t.Fatalf("one outage must refuse ONE row and promote the others: %+v", res.Counts)
	}
	if len(res.TransitionRefusals) != 1 || !strings.Contains(res.TransitionRefusals[0], "rb-doomed") ||
		!strings.Contains(res.TransitionRefusals[0], "injected store outage") {
		t.Fatalf("the refusal must name the row and the cause: %v", res.TransitionRefusals)
	}
	if err := verifyPromoted(ctx, st, res.Promoted); err != nil {
		t.Fatalf("the two promoted rows must verify: %v", err)
	}
	if got := st.statusOf(t, ids["rb-alpha"]); got != skillstore.StatusProduction {
		t.Fatalf("rb-alpha must be promoted, got %s", got)
	}
	if got := st.statusOf(t, ids["rb-omega"]); got != skillstore.StatusProduction {
		t.Fatalf("rb-omega must be promoted despite the earlier outage, got %s", got)
	}
	if got := st.statusOf(t, ids["rb-doomed"]); got != skillstore.StatusDraft {
		t.Fatalf("the outage row must remain draft, got %s", got)
	}
}

// TestOlderLeftoverDraftNeverSupersedesNewerProduction: the downgrade guard — when a NEWER seeded row
// already serves as production, a leftover older seeded draft skips instead of retiring it.
func TestOlderLeftoverDraftNeverSupersedesNewerProduction(t *testing.T) {
	ctx := context.Background()
	st := newMemPromoteStore()
	st.identity("rb-revised", skillstore.ClassRunbook)
	older := st.draft(t, "rb-revised", seedAuthor, "1.0.0")
	newer := st.draft(t, "rb-revised", seedAuthor, "1.1.0")
	if _, err := skillstore.Transition(ctx, st.m, st.lg, newer.ID, skillstore.StatusProduction, "fixture publish"); err != nil {
		t.Fatal(err)
	}

	res := runOnce(t, st)
	if res.Counts.Skipped != 1 || res.Counts.Promoted != 0 || res.Counts.Refused != 0 {
		t.Fatalf("the leftover older draft must SKIP: %+v", res.Counts)
	}
	if got := st.statusOf(t, newer.ID); got != skillstore.StatusProduction {
		t.Fatalf("the newer page must stay production, got %s", got)
	}
	if got := st.statusOf(t, older.ID); got != skillstore.StatusDraft {
		t.Fatalf("the older draft must stay draft, got %s", got)
	}
}

// TestConcurrentForeignIncumbentRefusedAtActTime pins the TOCTOU close: the plan is built while the
// promote is SAFE (no incumbent), then a foreign page becomes production in the window before execute.
// skillstore.Transition's incumbent-supersede would retire it with no author check, so the act-time
// re-decide MUST catch the foreign incumbent and refuse — never retire the live page. Without the
// re-decide in executePlan this reds (the foreign page would go retired and our row promoted).
func TestConcurrentForeignIncumbentRefusedAtActTime(t *testing.T) {
	ctx := context.Background()
	st := newMemPromoteStore()
	st.identity("rb-race", skillstore.ClassRunbook)
	ours := st.draft(t, "rb-race", seedAuthor, "1.0.0")

	p, err := buildPlan(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if c := p.counts(); c.Promoted != 1 {
		t.Fatalf("plan must promote while safe: %+v", c)
	}

	// The window: a concurrent writer publishes a FOREIGN draft to production for the same name.
	foreign := st.draft(t, "rb-race", "operator:someone-else", "0.9.0")
	if _, err := skillstore.Transition(ctx, st.m, st.lg, foreign.ID, skillstore.StatusProduction, "concurrent operator publish"); err != nil {
		t.Fatal(err)
	}

	res := executePlan(ctx, p, st)
	if res.Counts.Promoted != 0 || res.Counts.Refused != 1 {
		t.Fatalf("a foreign page that became production after planning must be refused at act time: %+v", res.Counts)
	}
	if len(res.TransitionRefusals) != 1 || !strings.Contains(res.TransitionRefusals[0], "diverged") {
		t.Fatalf("the refusal must name the divergence: %v", res.TransitionRefusals)
	}
	if got := st.statusOf(t, foreign.ID); got != skillstore.StatusProduction {
		t.Fatalf("the concurrently-published foreign page must STAY production, got %s", got)
	}
	if got := st.statusOf(t, ours.ID); got != skillstore.StatusDraft {
		t.Fatalf("our draft must stay draft (never promoted over a stranger's page), got %s", got)
	}
}

// TestConcurrentPromoteOfOurRowIsBenignSkip: if a concurrent run already promoted our exact planned
// row, the act-time re-decide sees it as production and SKIPS (benign) rather than refusing — the work
// is done, the exit code must not go non-zero.
func TestConcurrentPromoteOfOurRowIsBenignSkip(t *testing.T) {
	ctx := context.Background()
	st := newMemPromoteStore()
	st.identity("rb-doubled", skillstore.ClassRunbook)
	ours := st.draft(t, "rb-doubled", seedAuthor, "1.0.0")

	p, err := buildPlan(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	// A concurrent run promotes our exact row before we act.
	if _, err := skillstore.Transition(ctx, st.m, st.lg, ours.ID, skillstore.StatusProduction, "concurrent seeded promote"); err != nil {
		t.Fatal(err)
	}
	res := executePlan(ctx, p, st)
	if res.Counts.Skipped != 1 || res.Counts.Promoted != 0 || res.Counts.Refused != 0 {
		t.Fatalf("an already-promoted planned row must be a benign skip, not a refusal: %+v", res.Counts)
	}
}

// TestOperatorDecisionStands: a seeded runbook row an operator REJECTED (or retired) is a decision,
// not a pending draft — the name skips with the status named; the tool never resurrects it.
func TestOperatorDecisionStands(t *testing.T) {
	ctx := context.Background()
	st := newMemPromoteStore()
	st.identity("rb-rejected", skillstore.ClassRunbook)
	d := st.draft(t, "rb-rejected", seedAuthor, "1.0.0")
	if _, err := skillstore.Transition(ctx, st.m, st.lg, d.ID, skillstore.StatusRejected, "operator rejects the draft"); err != nil {
		t.Fatal(err)
	}

	res := runOnce(t, st)
	if res.Counts.Skipped != 1 || res.Counts.Promoted != 0 || res.Counts.Refused != 0 {
		t.Fatalf("a rejected seeded row must SKIP (the decision stands): %+v", res.Counts)
	}
	p, err := buildPlan(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Items[0].Reason, "rejected") {
		t.Fatalf("the skip must name the operator's decision: %+v", p.Items[0])
	}
	if got := st.statusOf(t, d.ID); got != skillstore.StatusRejected {
		t.Fatalf("the rejected row must stay rejected, got %s", got)
	}
}

// TestRevisedSeededDraftSupersedesOurOwnPage: the legitimate revision lane — a NEWER seeded draft over
// our OWN seeded production page promotes, and the incumbent retires (REQ-1302); the wiki serves
// exactly one production page per runbook name.
func TestRevisedSeededDraftSupersedesOurOwnPage(t *testing.T) {
	st := newMemPromoteStore()
	st.identity("rb-revised", skillstore.ClassRunbook)
	v1 := st.draft(t, "rb-revised", seedAuthor, "1.0.0")

	if res := runOnce(t, st); res.Counts.Promoted != 1 {
		t.Fatalf("first run: %+v", res.Counts)
	}
	v2 := st.draft(t, "rb-revised", seedAuthor, "1.1.0")
	res := runOnce(t, st)
	if res.Counts.Promoted != 1 || res.Counts.Refused != 0 {
		t.Fatalf("the revised seeded draft must promote: %+v", res.Counts)
	}
	if got := st.statusOf(t, v2.ID); got != skillstore.StatusProduction {
		t.Fatalf("v2 must be production, got %s", got)
	}
	if got := st.statusOf(t, v1.ID); got != skillstore.StatusRetired {
		t.Fatalf("the superseded v1 must be retired (one-production invariant), got %s", got)
	}
}
