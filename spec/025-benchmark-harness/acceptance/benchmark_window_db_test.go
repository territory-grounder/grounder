package acceptance

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/axis"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/judge"
)

// TG-632 — THE G4 REPORT SEAM, END TO END over a real database: axis.BenchmarkWindow (what
// `axisscore -benchmark-window` calls) reads the census, one scoped aggregate per site and the estate
// aggregate, and builds the verdict. The pieces are tested on their own in core/db and core/axis; this proves
// they are wired to each other — that the report's n for a site is the scoped read's n for that site.
//
// It lives HERE, not in cmd/axisscore, because the CI `harness` job runs this package with TG_TEST_DSN set; a
// DSN-gated test anywhere else would run in no pipeline at all.
//
// THE FIXTURE (a private 15-day window somewhere in 1950–1969, away from core/db's 1990–2009 fixtures; nothing
// is deleted, the window is unique per run):
//
//	dc1: nlA  day 1   agent loop, mutated + confirmed clear, judged 4.0, harness H
//	        nlB  day 14  agent loop, not mutated, harness H                    -> NL incidents span 13 days
//	        nlD  day 7   DETERMINISTIC lane, mutated, no loop, harness ""
//	dc2: grA  day 5   agent loop, mutated, harness H                         -> GR incidents span 0 days
//	faults: one at an dc1… host (detected by nlA's alert), one on a dc2… node (missed)
//
// Hand-derived: FAIL (n < 30 everywhere; GR's data spans 0 days, NL's does not); NL A3 gated on the agent-loop
// n=1 with pooled 2 / deterministic 1 beside it; estate A8 n = 3 actuated mutations; the harness check sees H×3
// and puts nlD on the deterministic line; one rubric (the binary's).
func TestBenchmarkWindowEndToEnd(t *testing.T) {
	dsn := os.Getenv("TG_TEST_DSN")
	if dsn == "" {
		t.Skip("TG_TEST_DSN not set — the G4 report seam needs a migrated Postgres (the CI harness job supplies one)")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	since := time.Date(1950, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(rand.Int64N(19*365*24*60)) * time.Minute)
	until := since.Add(15 * 24 * time.Hour)
	run := fmt.Sprintf("tg632e2e-%d", time.Now().UnixNano())
	day := func(d int) time.Time { return since.Add(time.Duration(d)*24*time.Hour + time.Hour) }
	harness := strings.Repeat("cd", 32)
	exec := func(what, q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
	}
	for _, r := range []struct {
		ref, host, site, tier, fp string
		mutated, clear            bool
		steps, ms                 int
		at                        time.Time
	}{
		{"nlA", "dc1" + run + "-a", "dc1", "primary", harness, true, true, 3, 4000, day(1)},
		{"nlB", "dc1" + run + "-b", "dc1", "primary", harness, false, false, 2, 5000, day(14)},
		{"nlD", "dc1" + run + "-d", "dc1", "deterministic-heal", "", true, false, 0, 0, day(7)},
		{"grA", "dc2" + run + "-a", "dc2", "primary", harness, true, false, 4, 6000, day(5)},
	} {
		exec("triage "+r.ref, `
			INSERT INTO session_triage (external_ref, host, alert_rule, band, op, op_class, proposed, predicted, outcome,
			                            conclusion, step_count, decision_ms, mutated, confirmed_clear, harness_fp,
			                            decision_model_tier, created_at)
			VALUES ($1,$2,'Device-Down','AUTO','start','start-guest',true,false,'ok','c',$3,$4,$5,$6,$7,$8,$9)`,
			run+"-"+r.ref, r.host, r.steps, r.ms, r.mutated, r.clear, r.fp, r.tier, r.at)
		exec("alert "+r.ref, `
			INSERT INTO ingest_alert (external_ref, source_type, alert_rule, host, site, received_at)
			VALUES ($1,'librenms','Device-Down',$2,$3,$4)`, run+"-"+r.ref, r.host, r.site, r.at)
	}
	exec("judgment", `INSERT INTO session_judgment (external_ref, dimension, score, rubric_version)
		VALUES ($1,'evidence_grounded',4,$2)`, run+"-nlA", judge.RubricVersion())
	exec("fault nl", `INSERT INTO injected_fault (host, fault_type, note, node, injected_at)
		VALUES ($1,'device-down','tg632 e2e','',$2)`, "dc1"+run+"-a", day(1).Add(-time.Minute))
	exec("fault gr", `INSERT INTO injected_fault (host, fault_type, note, node, injected_at)
		VALUES ($1,'device-down','tg632 e2e',$2,$3)`, run+"-gronly", "dc2"+run+"-pve", day(5))

	r, err := axis.BenchmarkWindowBar(ctx, db.NewAxisReadStore(pool), axis.BarG4, since, until, judge.RubricVersion())
	if err != nil {
		t.Fatalf("BenchmarkWindow: %v", err)
	}
	cell := func(scope, ax, name string) axis.BenchmarkCell {
		t.Helper()
		for _, c := range r.Cells {
			if c.Scope == scope && c.Axis == ax && c.Name == name {
				return c
			}
		}
		t.Fatalf("no cell %s %s %q", scope, ax, name)
		return axis.BenchmarkCell{}
	}
	n := func(c axis.BenchmarkCell) int {
		if c.N == nil {
			return -1
		}
		return *c.N
	}

	if r.Passed() || !r.WindowOK {
		t.Errorf("verdict %s windowOK %v — a 15-day window with n < 30 must FAIL on n, not on its length", r.Verdict, r.WindowOK)
	}
	all := strings.Join(r.Failures, "\n")
	if !strings.Contains(all, "dc2: data spans 0.0 days") || strings.Contains(all, "dc1: data spans") {
		t.Errorf("data span: GR (one incident) must fail, NL (13 days) must not:\n%s", all)
	}
	if r.IncidentsBySite["dc1"] != 3 || r.IncidentsBySite["dc2"] != 1 {
		t.Errorf("incidents by site = %v, want dc1=3 dc2=1", r.IncidentsBySite)
	}
	if a3 := cell("dc1", "A3", "heal success"); n(a3) != 1 || a3.SideN["pooled"] != 2 || a3.SideN["deterministic_lane"] != 1 {
		t.Errorf("NL A3 = n=%d side=%v, want the agent-loop n=1 gated with pooled 2 / deterministic 1 beside it", n(a3), a3.SideN)
	}
	if d := cell("dc1", "A6", "deterministic lane"); n(d) != 1 {
		t.Errorf("NL deterministic lane n=%d, want 1", n(d))
	}
	if a1, g1 := cell("dc1", "A1", "detection recall"), cell("dc2", "A1", "detection recall"); n(a1) != 1 || n(g1) != 1 ||
		a1.Value == nil || *a1.Value != 1 || g1.Value == nil || *g1.Value != 0 {
		t.Errorf("A1: NL n=%d value=%v, GR n=%d value=%v — want 1/1.0 (detected) and 1/0.0 (missed, placed by its node)",
			n(a1), a1.Value, n(g1), g1.Value)
	}
	if a2 := cell("dc1", "A2", "diagnosis correctness"); n(a2) != 1 || r.ScoredRubric != judge.RubricVersion() || !r.RubricOK {
		t.Errorf("A2 n=%d scored rubric %q uniform %v, want 1 under the binary's rubric", n(a2), r.ScoredRubric, r.RubricOK)
	}
	if !r.HarnessOK || r.HarnessDeterministic != 1 || len(r.HarnessFPs) != 1 || r.HarnessFPs[0].N != 3 {
		t.Errorf("harness: ok=%v fps=%v deterministic=%d — want H×3 uniform with nlD on its own line",
			r.HarnessOK, r.HarnessFPs, r.HarnessDeterministic)
	}
	if a8 := cell("estate", "A8", "safety-violation count"); n(a8) != 3 {
		t.Errorf("estate A8 n=%d, want 3 actuated mutations (nlA, nlD, grA)", n(a8))
	}
	if a8 := cell("dc2", "A8", "safety-violation count"); a8.N != nil || a8.Status != axis.StatusNotAttributable {
		t.Errorf("per-site A8 = %+v, want no n, NOT-SITE-ATTRIBUTABLE", a8)
	}
	if !strings.Contains(r.Text(), "VERDICT: FAIL") {
		t.Error("the text rendering does not carry the verdict")
	}
}
