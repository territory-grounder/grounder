package db

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

// TG-670 — THE A4 NUMERATOR, THE STOP SPLIT AND THE HARNESS CENSUS LANES, over a real Postgres.
//
// Three measurement defects the G4A program found (F10, F12), each a join or a predicate a fake cannot
// exercise, so — like axis_scope_test.go — this runs against every migration, gated on TG_TEST_DSN.
//
// THE FIXTURE (one private 2h window per run in 1970–1989, away from the other fixtures; rows accrete):
//
//	ref     outcome                          stop_reason                      band        proposed steps harness  lane
//	ok1     proposed                         —                                AUTO        yes      2     H        loop
//	fb1     proposed                         —                                POLL_PAUSE  yes      3     H        loop   + ledger fallback:autonomous-approve (in window)
//	fbh1    proposed                         —                                POLL_PAUSE  yes      3     H        loop   + ledger fallback:autonomous-hold
//	fbold   proposed                         —                                POLL_PAUSE  yes      3     H        loop   + ledger approve 3 DAYS BEFORE the window (outside the slack: not counted)
//	fbboth  proposed                         —                                POLL_PAUSE  yes      3     H        loop   + ledger hold, then approve (counted once, as released)
//	good1   no-proposal:stop                 agent requested stop             —           no       2     H        loop
//	bad1    no-proposal:stop                 unparseable model output         —           no       1     H        loop
//	bad2    no-proposal:stop                 confidence below stop threshold  —           no       4     H        loop
//	skip1   no-proposal:deterministic-skip   —                                —           no       0     ""       deterministic (tier)
//	esc1    escalated:handoff-limit          handoff poll limit reached       —           no       6     H        loop
//	fail1   failed:investigate               investigate activity failed      —           no       10    ""       INFRA-FAILED (back-filled steps)
//	fail2   failed:investigate               investigate activity failed      —           no       3     H        loop (it composed; unknown ≠ compatible)
//	unrec1  no-proposal:stop                 agent requested stop             —           no       2     ""       loop, UNRECORDED "" (against uniformity)
//	nc1     suppressed:duplicate             —                                —           no       0     ""       not-composed
//
//	ledger DECOY that must NOT count: an approve row naming a ref with no session.
//
// Every expected value below is read off this table by hand, never captured from the implementation.
type a4Fixture struct {
	base, until time.Time
	run         string
	harness     string
}

func (f a4Fixture) ref(short string) string { return f.run + "-" + short }

func seedA4Fixture(ctx context.Context, t *testing.T, p *Pool) a4Fixture {
	t.Helper()
	off := time.Duration(rand.Int64N(20*365*24*60)) * time.Minute
	f := a4Fixture{base: time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC).Add(off), run: fmt.Sprintf("tg670-%d", time.Now().UnixNano()),
		harness: strings.Repeat("ef", 32)}
	f.until = f.base.Add(2 * time.Hour)
	exec := func(what, q string, args ...any) {
		t.Helper()
		if _, err := p.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
	}
	at := func(m int) time.Time { return f.base.Add(time.Duration(m) * time.Minute) }
	type row struct {
		short, outcome, stop, band string
		proposed                   bool
		steps                      int
		fp                         string
	}
	rows := []row{
		{"ok1", "proposed", "", "AUTO", true, 2, f.harness},
		{"fb1", "proposed", "", "POLL_PAUSE", true, 3, f.harness},
		{"fbh1", "proposed", "", "POLL_PAUSE", true, 3, f.harness},
		{"fbold", "proposed", "", "POLL_PAUSE", true, 3, f.harness},
		{"fbboth", "proposed", "", "POLL_PAUSE", true, 3, f.harness},
		{"good1", "no-proposal:stop", goodStopReason, "", false, 2, f.harness},
		{"bad1", "no-proposal:stop", "unparseable model output", "", false, 1, f.harness},
		{"bad2", "no-proposal:stop", "confidence below stop threshold", "", false, 4, f.harness},
		{"skip1", deterministicSkipOutcome, "", "", false, 0, ""},
		{"esc1", "escalated:handoff-limit", "handoff poll limit reached", "", false, 6, f.harness},
		{"fail1", "failed:investigate", "investigate activity failed: activity error", "", false, 10, ""},
		{"fail2", "failed:investigate", "investigate activity failed: activity error", "", false, 3, f.harness},
		{"unrec1", "no-proposal:stop", goodStopReason, "", false, 2, ""},
		{"nc1", "suppressed:duplicate", "", "", false, 0, ""},
	}
	for i, r := range rows {
		host := "dc1" + f.run + "-" + r.short
		exec("triage "+r.short, `
			INSERT INTO session_triage (external_ref, host, alert_rule, band, op, op_class, proposed, predicted,
			                            outcome, stop_reason, conclusion, step_count, decision_ms, mutated, confirmed_clear,
			                            actor_attribution, harness_fp, created_at)
			VALUES ($1,$2,'Service up/down',$3,'start','start-service',$4,false,$5,$6,'c',$7,1000,false,false,'',$8,$9)`,
			f.ref(r.short), host, r.band, r.proposed, r.outcome, r.stop, r.steps, r.fp, at(10+i))
		exec("alert "+r.short, `
			INSERT INTO ingest_alert (external_ref, source_type, alert_rule, host, site, received_at)
			VALUES ($1,'librenms','Service up/down',$2,'dc1',$3)`, f.ref(r.short), host, at(10+i))
	}
	exec("skip1 tier", `UPDATE session_triage SET decision_model_tier = $1 WHERE external_ref = $2`, deterministicHealTier, f.ref("skip1"))

	// Fallback-approver rows, chained onto the ledger's current tail so the chain stays valid, with explicit
	// created_at: the in-window approve (fb1) and hold (fbh1), a hold THEN an approve for fbboth (counted once, as
	// released), a decoy naming no session, and an approve for fbold three days BEFORE the window — outside
	// fallbackLedgerSlack, so fbold must read as un-resolved and the slack's lower bound is load-bearing.
	seq, hash, err := NewLedgerStore(p).Tail(ctx)
	if err != nil {
		t.Fatalf("ledger tail: %v", err)
	}
	l := audit.NewLedgerFromTail(seq, hash)
	for _, e := range []struct {
		decision, ref string
		m             int
	}{
		{"fallback:autonomous-approve:ood-novel-incident", f.ref("fb1"), 40},
		{"fallback:autonomous-hold:server-derived-destructive-op", f.ref("fbh1"), 41},
		{"fallback:autonomous-hold:actor-attributed-authorized", f.ref("fbboth"), 42},
		{"fallback:autonomous-approve:ood-novel-incident", f.ref("fbboth"), 43},
		{"fallback:autonomous-approve:ood-novel-incident", f.ref("decoy-no-session"), 44},
		{"fallback:autonomous-approve:ood-novel-incident", f.ref("fbold"), -3 * 24 * 60},
	} {
		entry, err := l.Append(audit.GovDecision{Decision: e.decision,
			Reason: fallbackReasonRefPrefix + e.ref + " voter=tg:fallback-approver", ActionID: f.run + "-shape"})
		if err != nil {
			t.Fatalf("chain %s: %v", e.decision, err)
		}
		exec("ledger "+e.decision, `
			INSERT INTO governance_ledger (seq, decision, reason, action_id, withheld, prev_hash, hash, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			entry.Seq, entry.Decision, entry.Reason, entry.ActionID, entry.Withheld, entry.PrevHash, entry.Hash, at(e.m))
	}
	return f
}

func openA4Fixture(t *testing.T) (context.Context, *Pool, a4Fixture) {
	t.Helper()
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(p.Close)
	return ctx, p, seedA4Fixture(ctx, t, p)
}

func TestAxisRead_A4FallbackAndStopSplit_GoldenFixture(t *testing.T) {
	ctx, p, f := openA4Fixture(t)
	// An OPEN-ENDED window ($2 NULL) must read the same rows without erroring: the ledger read's upper bound
	// short-circuits on the NULL. The count is >= because everything after the fixture window is in it too.
	if open, err := NewAxisReadStore(p).AggregateScoped(ctx, AxisScope{Since: f.base}); err != nil || open.FallbackApproved < 2 {
		t.Errorf("open-ended window: err=%v fallbackApproved=%d, want nil and >= 2", err, open.FallbackApproved)
	}
	for _, scope := range []AxisScope{{Since: f.base, Until: f.until}, {Since: f.base, Until: f.until, Site: "dc1"}} {
		a, err := NewAxisReadStore(p).AggregateScoped(ctx, scope)
		if err != nil {
			t.Fatalf("aggregate %+v: %v", scope, err)
		}
		if a.Total != 14 || a.Proposed != 5 {
			t.Errorf("%s: total=%d proposed=%d, want 14/5", scope.Site, a.Total, a.Proposed)
		}
		// good1 + unrec1 are considered stand-downs; bad1 + bad2 are loops that failed to conclude; skip1 is neither.
		if a.AutonomousStops != 2 || a.FailedStops != 2 || a.EscalatedHandoffs != 1 {
			t.Errorf("%s: autonomousStops=%d failedStops=%d escalated=%d, want 2/2/1 — only %q is a considered stand-down, "+
				"the deterministic skip is neither", scope.Site, a.AutonomousStops, a.FailedStops, a.EscalatedHandoffs, goodStopReason)
		}
		if a.StopReasons[goodStopReason] != 2 || a.StopReasons["unparseable model output"] != 1 ||
			a.StopReasons["confidence below stop threshold"] != 1 || a.StopReasons["(none recorded)"] != 1 || len(a.StopReasons) != 4 {
			t.Errorf("%s: StopReasons = %v, want {%s:2 unparseable:1 confidence:1 (none recorded):1}", scope.Site, a.StopReasons, goodStopReason)
		}
		// fb1 and fbboth released (fbboth's earlier hold is superseded, counted once), fbh1 held; the decoy names no
		// session and fbold's 3-day-old approve is outside the slack.
		if a.FallbackApproved != 2 || a.FallbackHeld != 1 {
			t.Errorf("%s: fallbackApproved=%d held=%d, want 2/1 — a decoy ref or an out-of-slack row leaked in, a held-then-"+
				"released ref was counted twice, or the reason parse lost the ref", scope.Site, a.FallbackApproved, a.FallbackHeld)
		}
		if a.DeterministicSkips != 1 {
			t.Errorf("%s: deterministicSkips=%d, want 1 (skip1) — a classifier-decided skip is a recorded decision, credited on "+
				"its own line", scope.Site, a.DeterministicSkips)
		}
		if a.Bands["AUTO"] != 1 || a.Bands["POLL_PAUSE"] != 4 || a.DeterministicHeals != 1 {
			t.Errorf("%s: bands=%v deterministic=%d, want AUTO=1 POLL_PAUSE=4 and 1", scope.Site, a.Bands, a.DeterministicHeals)
		}
	}
}

func TestWindowCensus_InfraFailedIsItsOwnLane(t *testing.T) {
	ctx, p, f := openA4Fixture(t)
	c, err := NewAxisReadStore(p).WindowCensus(ctx, f.base, f.until)
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	// ok1 fb1 fbh1 fbold fbboth good1 bad1 bad2 esc1 fail2 under H (fail2 composed a harness and stays under it);
	// unrec1 ran the loop and recorded nothing: UNRECORDED, against uniformity.
	if c.HarnessFPs[f.harness] != 10 || c.HarnessFPs[""] != 1 || len(c.HarnessFPs) != 2 {
		t.Errorf("HarnessFPs = %v, want H×10 and \"\"×1 (unrec1) — a failed row WITH a fingerprint stays under it, an "+
			"unrecorded looped row still counts against uniformity", c.HarnessFPs)
	}
	if c.HarnessInfraFailed != 1 || c.HarnessNotComposed != 1 || c.HarnessDeterministic != 1 {
		t.Errorf("lanes: infraFailed=%d notComposed=%d deterministic=%d, want 1/1/1 (fail1 / nc1 / skip1)",
			c.HarnessInfraFailed, c.HarnessNotComposed, c.HarnessDeterministic)
	}
}

// MUTATION CONTROLS: perturb the SHIPPED SQL constants and require the fixture to notice (REQ-2512).
func TestAxisRead_A4AndCensus_MutationControls(t *testing.T) {
	ctx, p, f := openA4Fixture(t)
	store := NewAxisReadStore(p)
	sa, err := store.resolveScope(ctx, AxisScope{Since: f.base, Until: f.until})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	perturb := func(src, arm string) string {
		t.Helper()
		if !strings.Contains(src, arm) {
			t.Fatalf("the shipped SQL no longer contains %q — this control would pass vacuously", arm)
		}
		return strings.Replace(src, arm, "("+arm+" OR true)", 1)
	}
	fallback := func(sql string) (int, int) {
		t.Helper()
		var approved, held int
		if err := p.Pool.QueryRow(ctx, sql, sa.triage(fallbackReasonRefPrefix, fallbackLedgerSlack)...).Scan(&approved, &held); err != nil {
			t.Fatalf("fallback query: %v", err)
		}
		return approved, held
	}
	if a, h := fallback(fallbackResolutionsSQL); a != 2 || h != 1 {
		t.Fatalf("baseline fallback = %d/%d, want 2/1", a, h)
	}
	// The ref equality is the join: without it every proposal inherits every fallback row — all 5 read as
	// released (an approve row exists somewhere), and none as held (held excludes a released ref). All three
	// EXISTS arms carry the clause, so all three are neutralised.
	if !strings.Contains(fallbackResolutionsSQL, "fb.ref = t.external_ref") {
		t.Fatal("the shipped fallback SQL no longer joins on fb.ref = t.external_ref — this control would pass vacuously")
	}
	unjoined := strings.ReplaceAll(fallbackResolutionsSQL, "fb.ref = t.external_ref", "(fb.ref = t.external_ref OR true)")
	if a, h := fallback(unjoined); a != 5 || h != 0 {
		t.Errorf("dropping the ref equality gave %d/%d, want 5/0 — the fixture cannot see a cross-session leak", a, h)
	}
	// The slack's lower bound keeps fbold's 3-day-old approve out of the window; dropping it lets fbold in (3).
	if a, _ := fallback(perturb(fallbackResolutionsSQL, "l.created_at >= $1 - $7::interval")); a != 3 {
		t.Errorf("dropping the slack lower bound gave approved=%d, want 3 — fbold's stale approve should have leaked in", a)
	}
	// The release-wins rule: without the NOT EXISTS, fbboth (held, then released) is counted as held too (2).
	withoutReleaseWins := strings.Replace(fallbackResolutionsSQL, "AND NOT EXISTS (SELECT 1 FROM fb WHERE fb.ref = t.external_ref\n\t\t                                            AND fb.decision LIKE 'fallback:autonomous-approve:%')", "", 1)
	if withoutReleaseWins == fallbackResolutionsSQL {
		t.Fatal("the shipped fallback SQL no longer carries the release-wins NOT EXISTS — this control would pass vacuously")
	}
	if _, h := fallback(withoutReleaseWins); h != 2 {
		t.Errorf("dropping the release-wins clause gave held=%d, want 2 — a held-then-released ref must count once, as released", h)
	}

	// The census lane CASE: neutralising the outcome test moves every "" row into infra-failed, so the
	// UNRECORDED looped row (unrec1) vanishes from the uniformity check — exactly the silent reclassification
	// the rule forbids.
	lanes := func(caseSQL string) map[string]int {
		t.Helper()
		rows, err := p.Pool.Query(ctx, `
			SELECT harness_fp, `+caseSQL+`, count(DISTINCT external_ref)
			  FROM session_triage
			 WHERE created_at >= $1 AND created_at < $2 AND external_ref LIKE $4
			 GROUP BY 1, 2`, f.base, f.until, deterministicHealTier, f.run+"-%")
		if err != nil {
			t.Fatalf("lanes: %v", err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var fp, lane string
			var n int
			if err := rows.Scan(&fp, &lane, &n); err != nil {
				t.Fatalf("lanes scan: %v", err)
			}
			if lane == "loop" {
				out["loop:"+fp] += n
			} else {
				out[lane] += n
			}
		}
		return out
	}
	base := lanes(harnessLaneCase)
	if base["loop:"] != 1 || base["infra-failed"] != 1 || base["not-composed"] != 1 || base["deterministic"] != 1 {
		t.Fatalf("baseline lanes = %v", base)
	}
	if got := lanes(perturb(harnessLaneCase, "outcome LIKE 'failed:%'")); got["loop:"] != 0 || got["infra-failed"] != 3 {
		t.Errorf("neutralising the outcome test gave %v — the unrecorded looped row should have been swallowed into "+
			"infra-failed, which is the defect this control exists to catch", got)
	}
}

// goodStopReason must be the agent loop's own spelling, or every considered stand-down silently becomes a
// failed stop. core/db cannot import the agent, so read its source (the TestDeterministicHealTierMatchesTheRunner pattern).
func TestGoodStopReasonMatchesTheAgentLoop(t *testing.T) {
	src, err := os.ReadFile("../../agent/loop.go")
	if err != nil {
		t.Fatalf("read the agent loop: %v", err)
	}
	if !strings.Contains(string(src), `"`+goodStopReason+`",`) {
		t.Errorf("agent/loop.go StopReasons() no longer lists %q — core/db's copy has drifted", goodStopReason)
	}
	rsrc, err := os.ReadFile("../../temporal/runner/activities.go")
	if err != nil {
		t.Fatalf("read the runner activities: %v", err)
	}
	if !strings.Contains(string(rsrc), deterministicSkipOutcome) {
		t.Errorf("temporal/runner/activities.go no longer writes %q — core/db's copy has drifted", deterministicSkipOutcome)
	}
}

// fallbackReasonRefPrefix must be how the runner's durable-approval record names the session, or the A4
// fallback join silently finds nothing.
func TestFallbackReasonFormatMatchesTheWriter(t *testing.T) {
	src, err := os.ReadFile("../../temporal/runner/activities.go")
	if err != nil {
		t.Fatalf("read the runner activities: %v", err)
	}
	if !strings.Contains(string(src), `reason := "`+fallbackReasonRefPrefix+`" + in.ExternalRef`) ||
		!strings.Contains(string(src), `reason += " voter=" + in.Voter`) {
		t.Errorf("temporal/runner/activities.go no longer writes `reason := %q + in.ExternalRef` followed by ` voter=` — "+
			"the fallback join parses that exact format", fallbackReasonRefPrefix)
	}
}
