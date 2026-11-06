package db

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
)

// GOLDEN-FIXTURE TEST FOR WHAT COUNTS AS A SHADOW PROPOSAL.
//
// WHY THIS EXISTS. The proposals plane selected `outcome = 'proposed:shadow'`. Measured against production
// on 2026-08-01, the spine held 3,202 triage rows: 1,990 with outcome 'proposed', and exactly ONE with
// 'proposed:shadow'. So the surface the board calls "on day zero the entire product" rendered a single row
// over 1,484 real un-executed proposals in 18 recurring shapes, and the counterfactual headline built on
// the same predicate would have read "TG would have addressed 1 of 2,699 incidents" when the honest figure
// was 1,748.
//
// Four unit oracles were green throughout, because every one of them used a FIXTURE that wrote
// 'proposed:shadow' — a value the runner emits on one branch out of three (workflow.go:310, the case where
// no op-class is registered), and which production writes once in 3,202 sessions. A fixture that supplies
// values production does not write cannot see what the operator sees; it only re-states the code's
// assumption back to itself. So this test seeds THE STRINGS THE RUNNER ACTUALLY EMITS, in production
// proportions, against real SQL.
//
// The contract under test is the console's own caption — "named, recorded, NEVER EXECUTED":
//   - every outcome beginning 'proposed' is a proposal, whether or not an op-class was registered
//   - a row that ACTUATED is not a shadow proposal, however it is labelled
//   - `mutated` alone cannot decide that: it has a documented, op-class-correlated back-fill gap (see
//     MarkMutated), so a row with a real action_execution is excluded on the evidence, not the flag
func TestShadowProposalPredicateMatchesWhatTheRunnerWrites(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()

	s := NewTriageStore(p)
	const pfx = "shadowpred-"
	cleanup := func() {
		_, _ = p.Exec(ctx, `DELETE FROM action_execution WHERE external_ref LIKE $1`, pfx+"%")
		_, _ = p.Exec(ctx, `DELETE FROM session_triage  WHERE external_ref LIKE $1`, pfx+"%")
	}
	cleanup()
	defer cleanup()

	// Outcome strings are copied from the runner, not invented here:
	//   workflow.go:310 -> "proposed:shadow"   (no registered op-class)
	//   workflow.go:495 -> "proposed"          (recorded at propose time, before the vote)
	//   workflow.go:763 -> "proposed"          (recorded after the execute path)
	// plus the two terminal non-proposals the spine also carries.
	seed := []struct {
		ref      string
		outcome  string
		mutated  bool
		executed bool // a real action_execution row exists
		want     bool // must this appear on the shadow plane?
	}{
		{pfx + "a", "proposed", false, false, true},         // the 1,484 case the old predicate hid
		{pfx + "b", "proposed", false, false, true},         //
		{pfx + "c", "proposed:shadow", false, false, true},  // the 1 case the old predicate found
		{pfx + "d", "proposed", true, true, false},          // actuated — "never executes" would be a lie
		{pfx + "e", "proposed", false, true, false},         // the MarkMutated gap: flag says no, spine says yes
		{pfx + "f", "no-proposal:stop", false, false, false}, // TG correctly stood down; not a proposal
		{pfx + "g", "already-remediated", false, false, false},
	}
	for _, r := range seed {
		if err := s.RecordTriage(ctx, judge.TriageRow{
			ExternalRef: r.ref, Host: "h1", AlertRule: "Service up/down", Band: "POLL_PAUSE",
			Outcome: r.outcome, Proposed: true, Op: "restart nginx", OpClass: "restart-proxy",
			Conclusion: "seeded", Mutated: r.mutated,
		}); err != nil {
			t.Fatalf("seed %s: %v", r.ref, err)
		}
		if r.executed {
			// unverifiable=true pairs with a NULL verdict per action_execution_verdict_pairing_chk; it is
			// also the honest shape here — the fixture asserts the action RAN, not how it turned out.
			if _, err := p.Exec(ctx, `
				INSERT INTO action_execution
					(action_id, external_ref, verdict, unverifiable, target_host, site, executed_at, schema_version)
				VALUES ($1, $2, NULL, true, 'h1', 'dc1', now(), 1)`, "act-"+r.ref, r.ref); err != nil {
				t.Fatalf("seed execution %s: %v", r.ref, err)
			}
		}
	}

	rows, err := s.ListShadowProposals(ctx, 500)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		if len(r.ExternalRef) >= len(pfx) && r.ExternalRef[:len(pfx)] == pfx {
			got[r.ExternalRef] = true
		}
	}
	for _, r := range seed {
		if got[r.ref] != r.want {
			verb := "must appear on the shadow plane"
			if !r.want {
				verb = "must NOT appear on the shadow plane"
			}
			t.Errorf("%s (outcome=%q mutated=%v executed=%v) %s, but it %s",
				r.ref, r.outcome, r.mutated, r.executed, verb,
				map[bool]string{true: "did", false: "did not"}[got[r.ref]])
		}
	}

	// The badge count must agree with the list over the same predicate. Two predicates that drift is how a
	// surface ends up saying "1,484" beside a list of 1.
	n, err := s.CountShadowProposals(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n < 3 {
		t.Errorf("count returned %d but the fixture alone contributes 3 shadow proposals — the count and "+
			"the list must share one predicate", n)
	}
}

// TestCounterfactualCountsEveryProposalAndSplitsWhatRan pins the headline's arithmetic against the same
// seeded reality. `addressed` answers "for how many incidents did TG produce a remedy" — every 'proposed*'
// outcome, not the rare no-op-class branch — and `executed` is the subset it was actually allowed to carry
// out, reported separately because blending them invites granting a capability that is partly already
// granted.
func TestCounterfactualCountsEveryProposalAndSplitsWhatRan(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()

	s := NewTriageStore(p)
	const pfx = "shadowcf-"
	cleanup := func() { _, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref LIKE $1`, pfx+"%") }
	cleanup()
	defer cleanup()

	seed := []struct {
		ref     string
		outcome string
		mutated bool
	}{
		{pfx + "a", "proposed", false},
		{pfx + "b", "proposed", false},
		{pfx + "c", "proposed", true},
		{pfx + "d", "proposed:shadow", false},
		{pfx + "e", "no-proposal:stop", false},
	}
	for _, r := range seed {
		if err := s.RecordTriage(ctx, judge.TriageRow{
			ExternalRef: r.ref, Host: "h1", AlertRule: "r", Band: "POLL_PAUSE", Outcome: r.outcome,
			Proposed: true, Op: "op", OpClass: "cls", Conclusion: "seeded", Mutated: r.mutated,
		}); err != nil {
			t.Fatalf("seed %s: %v", r.ref, err)
		}
	}

	since := time.Now().UTC().Add(-time.Hour)
	incidents, addressed, executed, err := s.CounterfactualSince(ctx, since)
	if err != nil {
		t.Fatalf("counterfactual: %v", err)
	}
	if incidents < 5 {
		t.Errorf("denominator must count EVERY triage session in the window including stand-downs; the "+
			"fixture contributes 5 and got %d", incidents)
	}
	if addressed < 4 {
		t.Errorf("addressed must count every 'proposed*' outcome (fixture contributes 4), got %d — "+
			"counting only 'proposed:shadow' is what made this headline read 1 of 2699 in production",
			addressed)
	}
	if executed < 1 {
		t.Errorf("executed must count the proposals that actuated (fixture contributes 1), got %d", executed)
	}
	if executed > addressed {
		t.Errorf("executed (%d) cannot exceed addressed (%d) — they must come from one scan", executed, addressed)
	}
}
