package db

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
)

// TG-632 — THE SCOPED AGGREGATE AND THE WINDOW CENSUS, over a real Postgres.
//
// The G4 bar needs every axis per SITE in one FIXED window, and session_triage has no site column: the site
// of an incident is its ingest_alert stamp, the site of an injected fault is the injector's own node/host
// record. Each of those is a join, and join semantics are exactly what a fake cannot reproduce — so, like
// axis_read_test.go, this runs against every migration, gated on TG_TEST_DSN.
//
// THE FIXTURE (one private window per run, far from every other fixture's rows; nothing is deleted — rows
// accrete, and the window is unique, so a re-run never reads a previous run's rows):
//
//	window W = [base, base+3h)
//	incident  host            ingest site   proposed op_class         mutated clear attrib       band       steps ms    harness
//	nl1       dc1…-a      dc1        yes      start-guest      yes     yes   test         AUTO       3     4000  H
//	nl2       dc1…-b      NL (legacy)   yes      restart-service  no      no    —            POLL_PAUSE 2     8000  H
//	gr1       dc2…-a      dc2        yes      start-service    yes     no    SUSPICIOUS   AUTO       4     9000  H
//	junk1     tg632…-junk     no            no       —                no      no    —            <none>     0     0     ""
//	orphan1   tg632…-orphan   (no ingest_alert row)  yes  start-guest no      no    —            AUTO       1     1000  H
//	unrec1    tg632…-unrec    tx            no       —                no      no    —            <none>     2     0     ""
//	grdet     dc2…-d      dc2        yes      start-guest      yes     no    —            AUTO       0     0     ""  (tier deterministic-heal)
//	txdet     tg632…-txdet    tx            yes      start-guest      no      no    —            AUTO       0     0     ""  (tier deterministic-heal)
//	nlEarly   dc1…-a      dc1        — at base-1m  (BEFORE the window: excluded)
//	nlLate    dc1…-a      dc1        — at base+3h  (== until, which is EXCLUSIVE: excluded)
//
//	judgments: nl1 4.0 and nl2 2.0 under the binary's rubric; gr1 5.0 under an OLD rubric
//	faults:    fA host dc1…-a, node ""             -> dc1  (device-down; nl1's alert 60s later DETECTS it)
//	           fB host dc2…-a, node ""             -> dc2  (disk-fill; no space/disk alert: a MISS)
//	           fC host tg632…-nodeonly, node dc2…  -> dc2  via the node (a MISS: the host never alerted)
//	           fD host tg632…-orphanf, node ""         -> UNATTRIBUTED
//	           fE host dc1…-c, node dc2…       -> UNATTRIBUTED (node and host disagree: refuse to guess)
//
//	fC and fE are deliberately asymmetric: ignoring the node moves fC out of GR and fE into NL, and dropping the
//	refusal moves fE into GR — so neither mutation can leave every per-site count unchanged.
//	executions: one at site 'dc1', one at the legacy spelling 'GR'
//	verdicts (executed, gate execute/pass): 'match' at dc1, 'deviation' at the legacy spelling 'gr'
//	blast predictions: dc1 tp=2 fp=1 fn=0; dc2 tp=0 fp=3 fn=1
//	poll reason 'tg632-novel' on nl2 only
//	DECOYS that must be excluded from every site (and the boundary ones from the estate too):
//	  AT until, dc1:      an execution, a 'deviation' verdict, a prediction, and poll reason 'tg632-late' on nl2
//	  in window, no site:    an execution and a 'partial' verdict at 'tx', a prediction at '', and poll reason
//	                         'tg632-junk' on junk1 (whose incident is stamped 'no')
//
//	harness: junk1 is "" with NO steps (never composed a harness: outside the check); unrec1 is "" WITH steps
//	(ran the loop, recorded nothing: counts as "unrecorded" against uniformity); grdet is the deterministic heal
//	lane (no loop, "" fingerprint): outside the check on its own line, and outside A6a/A6b with its own n.
//
// Every expected value below is read off this table by hand, never captured from the implementation.
type scopeFixture struct {
	base, until time.Time
	run         string
	hostNLA     string
}

func seedScopeFixture(ctx context.Context, t *testing.T, p *Pool) scopeFixture {
	t.Helper()
	now := time.Now().UTC()
	// A private window: a random minute in 1990–2009. Nothing else in the suite writes there, and two seeds
	// (two tests, two runs) collide only if their 3h windows overlap — ~3e-5 per pair.
	off := time.Duration(rand.Int64N(20*365*24*60)) * time.Minute
	f := scopeFixture{base: time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC).Add(off), run: fmt.Sprintf("tg632-%d", now.UnixNano())}
	f.until = f.base.Add(3 * time.Hour)
	f.hostNLA = "dc1" + f.run + "-a"
	hostNLB, hostGRA := "dc1"+f.run+"-b", "dc2"+f.run+"-a"
	harness := strings.Repeat("ab", 32)

	exec := func(what, q string, args ...any) {
		t.Helper()
		if _, err := p.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
	}
	triage := func(ref, host, rule, band, opClass string, proposed, mutated, clear bool, attrib string,
		steps, ms int, fp string, at time.Time) {
		t.Helper()
		exec("triage "+ref, `
			INSERT INTO session_triage (external_ref, host, alert_rule, band, op, op_class, proposed, predicted,
			                            outcome, conclusion, step_count, decision_ms, mutated, confirmed_clear,
			                            actor_attribution, harness_fp, created_at)
			VALUES ($1,$2,$3,$4,'start',$5,$6,false,'ok','c',$7,$8,$9,$10,$11,$12,$13)`,
			f.run+"-"+ref, host, rule, band, opClass, proposed, steps, ms, mutated, clear, attrib, fp, at)
	}
	alert := func(ref, host, site, rule string, at time.Time) {
		t.Helper()
		exec("alert "+ref, `
			INSERT INTO ingest_alert (external_ref, source_type, alert_rule, host, site, received_at)
			VALUES ($1,'librenms',$2,$3,$4,$5)`, f.run+"-"+ref, rule, host, site, at)
	}

	at := func(m int) time.Time { return f.base.Add(time.Duration(m) * time.Minute) }
	triage("nl1", f.hostNLA, "Device-Down", "AUTO", "start-guest", true, true, true, "authorized-test", 3, 4000, harness, at(10))
	alert("nl1", f.hostNLA, "dc1", "Device-Down", at(10))
	triage("nl2", hostNLB, "Service up/down", "POLL_PAUSE", "restart-service", true, false, false, "", 2, 8000, harness, at(20))
	alert("nl2", hostNLB, "NL", "Service up/down", at(20))
	triage("gr1", hostGRA, "Service up/down", "AUTO", "start-service", true, true, false, "attributed-suspicious", 4, 9000, harness, at(30))
	alert("gr1", hostGRA, "dc2", "Service up/down", at(30))
	triage("junk1", f.run+"-junk", "Watchdog", "", "", false, false, false, "", 0, 0, "", at(40))
	alert("junk1", f.run+"-junk", "no", "Watchdog", at(40))
	triage("orphan1", f.run+"-orphan", "Device-Down", "AUTO", "start-guest", true, false, false, "", 1, 1000, harness, at(50))
	triage("unrec1", f.run+"-unrec", "Watchdog", "", "", false, false, false, "", 2, 0, "", at(55))
	alert("unrec1", f.run+"-unrec", "tx", "Watchdog", at(55))
	triage("grdet", "dc2"+f.run+"-d", "Device-Down", "AUTO", "start-guest", true, true, false, "", 0, 0, "", at(57))
	exec("grdet tier", `UPDATE session_triage SET decision_model_tier = $1 WHERE external_ref = $2`,
		deterministicHealTier, f.run+"-grdet")
	alert("grdet", "dc2"+f.run+"-d", "dc2", "Device-Down", at(57))
	triage("txdet", f.run+"-txdet", "Device-Down", "AUTO", "start-guest", true, false, false, "", 0, 0, "", at(58))
	exec("txdet tier", `UPDATE session_triage SET decision_model_tier = $1 WHERE external_ref = $2`,
		deterministicHealTier, f.run+"-txdet")
	alert("txdet", f.run+"-txdet", "tx", "Device-Down", at(58))
	// The window's edges: one row a minute BEFORE since, one exactly AT until (exclusive) — both dc1.
	triage("nlEarly", f.hostNLA, "Device-Down", "AUTO", "start-guest", true, true, true, "", 5, 5000, harness, at(-1))
	alert("nlEarly", f.hostNLA, "dc1", "Device-Down", at(-1))
	triage("nlLate", f.hostNLA, "Device-Down", "AUTO", "start-guest", true, true, true, "", 5, 5000, harness, f.until)
	alert("nlLate", f.hostNLA, "dc1", "Device-Down", f.until)

	rubric := judge.RubricVersion()
	for _, j := range []struct {
		ref, rubric string
		score       float64
	}{{"nl1", rubric, 4}, {"nl2", rubric, 2}, {"gr1", "tg632-old-rubric", 5}} {
		exec("judgment "+j.ref, `
			INSERT INTO session_judgment (external_ref, dimension, score, rubric_version)
			VALUES ($1,'evidence_grounded',$2,$3)`, f.run+"-"+j.ref, j.score, j.rubric)
	}
	untilMin := int(f.until.Sub(f.base) / time.Minute)
	for _, r := range []struct {
		ref, reason string
		m           int
	}{{"nl2", "tg632-novel", 21}, {"nl2", "tg632-late", untilMin}, {"junk1", "tg632-junk", 41}} {
		exec("risk audit "+r.reason, `
			INSERT INTO session_risk_audit (external_ref, risk_level, band, signals_json, action_id, schema_version, created_at)
			VALUES ($1,'high','POLL_PAUSE',jsonb_build_object('poll_reason',$4::text),$2,1,$3)`,
			f.run+"-"+r.ref, f.run+"-act-"+r.reason, at(r.m), r.reason)
	}

	fault := func(host, node, class string, m int) {
		t.Helper()
		exec("fault "+host, `
			INSERT INTO injected_fault (host, fault_type, note, node, injected_at)
			VALUES ($1,$2,'tg632 scope fixture',$3,$4)`, host, class, node, at(m))
	}
	fault(f.hostNLA, "", "device-down", 9)                                    // fA: dc1, detected by nl1's alert at +10m
	fault(hostGRA, "", "disk-fill", 29)                                       // fB: dc2, missed
	fault(f.run+"-nodeonly", "dc2"+f.run+"-pve", "device-down", 31)       // fC: dc2 via node, missed
	fault(f.run+"-orphanf", "", "device-down", 32)                            // fD: unattributed
	fault("dc1"+f.run+"-c", "dc2"+f.run+"-pve", "device-down", 33)    // fE: conflict -> unattributed
	fault(f.hostNLA, "", "device-down", -5)                                   // before the window
	fault(f.hostNLA, "", "device-down", int(f.until.Sub(f.base)/time.Minute)) // AT until: excluded

	for _, e := range []struct {
		id, site string
		m        int
	}{{"nl", "dc1", 35}, {"gr", "GR", 35}, {"nl-late", "dc1", untilMin}, {"tx", "tx", 35}} {
		exec("execution "+e.id, `
			INSERT INTO action_execution (action_id, external_ref, verdict, unverifiable, target_host, site, executed_at)
			VALUES ($1,$2,'match'::verdict,false,'h',$3,$4)`, f.run+"-ex-"+e.id, f.run+"-ref-"+e.id, e.site, at(e.m))
	}
	for _, v := range []struct {
		id, site, verdict string
		m                 int
	}{{"nl", "dc1", "match", 36}, {"gr", "gr", "deviation", 36}, {"nl-late", "dc1", "deviation", untilMin}, {"tx", "tx", "partial", 36}} {
		exec("verdict "+v.id, `
			INSERT INTO action_verdict (action_id, plan_hash, verdict, site, schema_version, created_at)
			VALUES ($1,$5,$2::verdict,$3,1,$4)`, f.run+"-av-"+v.id, v.verdict, v.site, at(v.m), f.run+"-avplan-"+v.id)
		exec("gate "+v.id, `
			INSERT INTO interceptor_gate_verdict (action_id, ordinal, gate, verdict, created_at)
			VALUES ($1,1,'execute','pass',$2)`, f.run+"-av-"+v.id, at(v.m))
	}
	for _, b := range []struct {
		id, site   string
		tp, fp, fn int
		m          int
	}{{"nl", "dc1", 2, 1, 0, 37}, {"gr", "dc2", 0, 3, 1, 37}, {"nl-late", "dc1", 5, 5, 5, untilMin}, {"nosite", "", 1, 1, 1, 37}} {
		exec("prediction "+b.id, `
			INSERT INTO infragraph_prediction (plan_hash, action_id, target_host, site, prediction_hash, tp, fp, fn,
			                                   schema_version, committed_at)
			VALUES ($8,$1,'h',$2,$3,$4,$5,$6,1,$7)`,
			f.run+"-ip-"+b.id, b.site, f.run+"-ph-"+b.id, b.tp, b.fp, b.fn, at(b.m), f.run+"-plan-"+b.id)
	}
	return f
}

func openScopeFixture(t *testing.T) (context.Context, *Pool, scopeFixture) {
	t.Helper()
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(p.Close)
	return ctx, p, seedScopeFixture(ctx, t, p)
}

func TestAxisScope_SiteAndFixedWindow_GoldenFixture(t *testing.T) {
	ctx, p, f := openScopeFixture(t)
	store := NewAxisReadStore(p)

	nl, err := store.AggregateScoped(ctx, AxisScope{Since: f.base, Until: f.until, Site: "dc1"})
	if err != nil {
		t.Fatalf("dc1 aggregate: %v", err)
	}
	// nl1 + nl2 (the legacy "NL" stamp folds to dc1). NOT nlEarly (before since), NOT nlLate (AT until).
	if nl.Total != 2 {
		t.Errorf("dc1 Total = %d, want 2 — either the legacy NL spelling was dropped (1), or a window edge "+
			"leaked in (3+), or another site's incidents were counted", nl.Total)
	}
	if nl.Proposed != 2 || nl.OpClassProposals != 2 || strings.Join(nl.OpClasses, ",") != "restart-service,start-guest" {
		t.Errorf("dc1 A5: proposed=%d opClassProposals=%d classes=%v, want 2/2/[restart-service start-guest]",
			nl.Proposed, nl.OpClassProposals, nl.OpClasses)
	}
	if nl.MutatedCount != 1 || nl.HealConfirmedCount != 1 || nl.SuspiciousActuations != 0 {
		t.Errorf("dc1 A3/A7: mutated=%d clear=%d suspicious=%d, want 1/1/0 (gr1's suspicious actuation is GR's)",
			nl.MutatedCount, nl.HealConfirmedCount, nl.SuspiciousActuations)
	}
	if nl.StepsN != 2 || nl.MeanSteps != 2.5 || nl.DecisionN != 2 || nl.DecisionMedianMs != 6000 {
		t.Errorf("dc1 A6: steps n=%d mean=%v, decision n=%d median=%d, want 2/2.5 and 2/6000",
			nl.StepsN, nl.MeanSteps, nl.DecisionN, nl.DecisionMedianMs)
	}
	if nl.JudgedUnderRubric != 2 || nl.DimMeans["evidence_grounded"] != 3.0 {
		t.Errorf("dc1 A2: judged-under-rubric=%d mean=%v, want 2 and 3.0", nl.JudgedUnderRubric, nl.DimMeans["evidence_grounded"])
	}
	if nl.Bands["AUTO"] != 1 || nl.Bands["POLL_PAUSE"] != 1 || len(nl.PollReasons) != 1 || nl.PollReasons["tg632-novel"] != 1 {
		t.Errorf("dc1 A4: bands=%v pollReasons=%v, want AUTO=1 POLL_PAUSE=1 and exactly {tg632-novel:1} — "+
			"'tg632-late' sits AT until and 'tg632-junk' on an unattributed incident", nl.Bands, nl.PollReasons)
	}
	// A1: only fA is dc1's, and nl1's Device-Down alert 60s later detects it.
	if nl.InjectedFaults != 1 || nl.DetectedFaults != 1 {
		t.Errorf("dc1 A1: %d/%d detected/injected, want 1/1", nl.DetectedFaults, nl.InjectedFaults)
	}
	if len(nl.Verdicts) != 1 || nl.Verdicts["match"] != 1 || nl.PredScored != 1 || nl.PredTP != 2 || nl.PredFP != 1 {
		t.Errorf("dc1 A2 ground truth: verdicts=%v blast scored=%d tp=%d fp=%d, want {match:1} and 1/2/1",
			nl.Verdicts, nl.PredScored, nl.PredTP, nl.PredFP)
	}
	// The A8 census population is the window's ledger rows; nothing in this private 1990s window wrote one.
	if nl.LedgerWindowRows != 0 {
		t.Errorf("LedgerWindowRows = %d in a window no ledger row was written in — the window bound is gone", nl.LedgerWindowRows)
	}
	if nl.UnattributedFaults != 2 {
		t.Errorf("dc1 UnattributedFaults = %d, want 2 (fD has no site anywhere; fE's node and host disagree)", nl.UnattributedFaults)
	}

	gr, err := store.AggregateScoped(ctx, AxisScope{Since: f.base, Until: f.until, Site: "dc2"})
	if err != nil {
		t.Fatalf("dc2 aggregate: %v", err)
	}
	if gr.Total != 2 || gr.MutatedCount != 2 || gr.SuspiciousActuations != 1 || gr.HealConfirmedCount != 0 {
		t.Errorf("dc2: total=%d mutated=%d suspicious=%d clear=%d, want 2/2/1/0", gr.Total, gr.MutatedCount,
			gr.SuspiciousActuations, gr.HealConfirmedCount)
	}
	// The deterministic lane's share of the pooled A3/A4/A7 denominators (the report gates on the remainder).
	if gr.DeterministicProposals != 1 || gr.DeterministicMutated != 1 || nl.DeterministicProposals != 0 || nl.DeterministicMutated != 0 {
		t.Errorf("deterministic share: gr proposals=%d mutated=%d, nl %d/%d — want 1/1 and 0/0",
			gr.DeterministicProposals, gr.DeterministicMutated, nl.DeterministicProposals, nl.DeterministicMutated)
	}
	// The data-span inputs: each site's first and last attributed incident inside the window.
	if !nl.FirstIncident.Equal(f.base.Add(10*time.Minute)) || !nl.LastIncident.Equal(f.base.Add(20*time.Minute)) ||
		!gr.FirstIncident.Equal(f.base.Add(30*time.Minute)) || !gr.LastIncident.Equal(f.base.Add(57*time.Minute)) {
		t.Errorf("incident span: nl %s..%s gr %s..%s, want +10m..+20m and +30m..+57m (nlEarly/nlLate are outside)",
			nl.FirstIncident, nl.LastIncident, gr.FirstIncident, gr.LastIncident)
	}
	// The deterministic lane: counted on its own, and outside A6a/A6b because it ran no loop.
	if gr.DeterministicHeals != 1 || nl.DeterministicHeals != 0 || gr.StepsN != 1 || gr.DecisionN != 1 {
		t.Errorf("deterministic lane: gr=%d nl=%d, gr A6a n=%d A6b n=%d, want 1/0 and 1/1 (gr1 only)",
			gr.DeterministicHeals, nl.DeterministicHeals, gr.StepsN, gr.DecisionN)
	}
	// gr1 IS judged, but under another rubric: version-blind Judged sees it, A2's n does not.
	if gr.Judged != 1 || gr.JudgedUnderRubric != 0 || len(gr.DimMeans) != 0 {
		t.Errorf("dc2 A2: judged=%d judged-under-rubric=%d means=%v, want 1/0/none — a mean over two rubrics "+
			"is a number nobody can defend", gr.Judged, gr.JudgedUnderRubric, gr.DimMeans)
	}
	// Site leaks through the inline (non-constant) site arms: GR must see none of NL's composition, verdicts or
	// predictions, and must see its own legacy-spelled verdict.
	if len(gr.PollReasons) != 0 || len(gr.Verdicts) != 1 || gr.Verdicts["deviation"] != 1 ||
		gr.PredScored != 1 || gr.PredTP != 0 || gr.PredFP != 3 || gr.PredFN != 1 {
		t.Errorf("dc2: pollReasons=%v verdicts=%v blast scored=%d tp=%d fp=%d fn=%d, want none, {deviation:1}, 1/0/3/1",
			gr.PollReasons, gr.Verdicts, gr.PredScored, gr.PredTP, gr.PredFP, gr.PredFN)
	}
	// fB (host prefix) and fC (node only): both GR, both missed. The miss on a host that never alerted is the
	// case an ingest_alert-based attribution would have DROPPED, inflating GR recall to 0/1 or 0/0.
	if gr.InjectedFaults != 2 || gr.DetectedFaults != 0 {
		t.Errorf("dc2 A1: %d/%d detected/injected, want 0/2", gr.DetectedFaults, gr.InjectedFaults)
	}
	// The same rubric, scored explicitly: a window judged under the old rubric is still scoreable under it.
	grOld, err := store.AggregateScoped(ctx, AxisScope{Since: f.base, Until: f.until, Site: "dc2", Rubric: "tg632-old-rubric"})
	if err != nil {
		t.Fatalf("dc2 old-rubric aggregate: %v", err)
	}
	if grOld.JudgedUnderRubric != 1 || grOld.DimMeans["evidence_grounded"] != 5.0 {
		t.Errorf("dc2 under its own rubric: judged=%d mean=%v, want 1 and 5.0", grOld.JudgedUnderRubric, grOld.DimMeans["evidence_grounded"])
	}

	est, err := store.AggregateScoped(ctx, AxisScope{Since: f.base, Until: f.until})
	if err != nil {
		t.Fatalf("estate aggregate: %v", err)
	}
	// The estate keeps EVERY incident and fault, attributed or not — the unattributed are excluded only from sites.
	if est.Total != 8 || est.InjectedFaults != 5 || est.UnattributedFaults != 0 {
		t.Errorf("estate: total=%d faults=%d unattributed=%d, want 8/5/0", est.Total, est.InjectedFaults, est.UnattributedFaults)
	}
	// junk1 also ran no loop (step_count 0) but is NOT the deterministic lane: the lane is the tier, not "no steps".
	// txdet is the lane but was not mutated, so the lane's MUTATED share is grdet alone.
	if est.DeterministicHeals != 2 || est.DeterministicProposals != 2 || est.DeterministicMutated != 1 {
		t.Errorf("estate deterministic heals/proposals/mutated = %d/%d/%d, want 2/2/1 (grdet + txdet; junk1 ran no "+
			"loop but is not the lane; only grdet mutated)", est.DeterministicHeals, est.DeterministicProposals, est.DeterministicMutated)
	}
	// The estate keeps the unattributed ('tx', '') rows and drops ONLY the ones at until.
	if len(est.Verdicts) != 3 || est.Verdicts["match"] != 1 || est.Verdicts["deviation"] != 1 || est.Verdicts["partial"] != 1 ||
		est.PredScored != 3 {
		t.Errorf("estate verdicts=%v blast scored=%d, want {match:1 deviation:1 partial:1} and 3 — the 'deviation' and the "+
			"prediction AT until must be excluded, the unattributed ones kept", est.Verdicts, est.PredScored)
	}
	if len(est.PollReasons) != 2 || est.PollReasons["tg632-late"] != 0 || est.PollReasons["tg632-junk"] != 1 {
		t.Errorf("estate pollReasons=%v, want {tg632-novel:1 tg632-junk:1} — tg632-late sits AT until", est.PollReasons)
	}
	if nl.Total+gr.Total >= est.Total {
		t.Errorf("sites sum to %d of %d estate incidents — the unattributed junk/orphan incidents must be in NO site",
			nl.Total+gr.Total, est.Total)
	}

	if lb, err := store.LoopBypassScoped(ctx, AxisScope{Since: f.base, Until: f.until}); err != nil || lb.Executed != 3 {
		t.Errorf("estate loop-bypass Executed = %d (%v), want 3 (nl, GR, tx — not the one AT until)", lb.Executed, err)
	}
	// G6 through action_execution's own site column, legacy spelling folded.
	for site, want := range map[string]int{"dc1": 1, "dc2": 1} {
		lb, err := store.LoopBypassScoped(ctx, AxisScope{Since: f.base, Until: f.until, Site: site})
		if err != nil {
			t.Fatalf("loop-bypass %s: %v", site, err)
		}
		if lb.Executed != want {
			t.Errorf("loop-bypass %s Executed = %d, want %d (the 'GR' row must fold to dc2)", site, lb.Executed, want)
		}
	}
}

func TestAxisScope_RejectsAnUndeclaredSiteAndAnInvertedWindow(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	store := NewAxisReadStore(p)
	since := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.AggregateScoped(ctx, AxisScope{Since: since, Until: since.Add(time.Hour), Site: "GR"}); err == nil {
		t.Error("an undeclared site spelling was accepted — it would score nothing and read as a site with no incidents")
	}
	if _, err := store.AggregateScoped(ctx, AxisScope{Since: since, Until: since.Add(-time.Hour)}); err == nil {
		t.Error("until before since was accepted")
	}
}

func TestWindowCensus_GoldenFixture(t *testing.T) {
	ctx, p, f := openScopeFixture(t)
	c, err := NewAxisReadStore(p).WindowCensus(ctx, f.base, f.until)
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if c.TriagesBySite["dc1"] != 2 || c.TriagesBySite["dc2"] != 2 || len(c.TriagesBySite) != 2 {
		t.Errorf("TriagesBySite = %v, want dc1=2 dc2=2", c.TriagesBySite)
	}
	if c.TriagesUnattributed["no"] != 1 || c.TriagesUnattributed["tx"] != 2 || c.TriagesUnattributed[noIngestRowLabel] != 1 ||
		len(c.TriagesUnattributed) != 3 {
		t.Errorf("TriagesUnattributed = %v, want {no:1, tx:2, %s:1} — a junk Alertmanager label is not a site, and an "+
			"incident with no ingest row is counted, not lost", c.TriagesUnattributed, noIngestRowLabel)
	}
	if c.FaultsBySite["dc1"] != 1 || c.FaultsBySite["dc2"] != 2 || c.FaultsUnattributed != 2 {
		t.Errorf("faults by site = %v unattributed=%d, want dc1=1 dc2=2 and 2", c.FaultsBySite, c.FaultsUnattributed)
	}
	if len(c.RubricVersions) != 2 || c.RubricVersions[judge.RubricVersion()] != 2 || c.RubricVersions["tg632-old-rubric"] != 1 {
		t.Errorf("RubricVersions = %v, want the binary's ×2 and tg632-old-rubric ×1", c.RubricVersions)
	}
	// junk1 ("" + no steps) never composed a harness; unrec1 ("" + steps) ran the loop and recorded nothing.
	if len(c.HarnessFPs) != 2 || c.HarnessFPs[strings.Repeat("ab", 32)] != 4 || c.HarnessFPs[""] != 1 ||
		c.HarnessNotComposed != 1 || c.HarnessDeterministic != 2 {
		t.Errorf("HarnessFPs = %v notComposed=%d deterministic=%d, want the fixture fingerprint ×4, the unrecorded "+
			"\"\" ×1 (unrec1), 1 not-composed (junk1) and 2 deterministic (grdet, txdet)", c.HarnessFPs,
			c.HarnessNotComposed, c.HarnessDeterministic)
	}
	// Site-column rows with no declared site: the 'tx' execution and verdict, the '' prediction. The dc1 rows
	// AT until are outside the window and the attributed ones are not counted here.
	if len(c.OtherUnattributed) != 3 || c.OtherUnattributed["action_verdict"] != 1 ||
		c.OtherUnattributed["action_execution"] != 1 || c.OtherUnattributed["infragraph_prediction"] != 1 {
		t.Errorf("OtherUnattributed = %v, want action_verdict=1 action_execution=1 infragraph_prediction=1", c.OtherUnattributed)
	}
	if _, err := NewAxisReadStore(p).WindowCensus(ctx, f.base, time.Time{}); err == nil {
		t.Error("an open-ended census was accepted — a benchmark window must be fixed to be re-readable")
	}
}

// MUTATION CONTROLS (REQ-2501 / REQ-2512): perturb the SHIPPED scope constants, not a paraphrase, and require
// the fixture to tell the difference. Each perturbation neutralises one arm with "OR true" so the statement
// keeps every bound parameter (a dropped $n would fail to prepare, which proves nothing about the predicate).
func TestAxisScope_MutationControl_ShippedPredicatesAreLoadBearing(t *testing.T) {
	ctx, p, f := openScopeFixture(t)
	sa, err := NewAxisReadStore(p).resolveScope(ctx, AxisScope{Since: f.base, Until: f.until, Site: "dc1"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	countTriages := func(latest string) int {
		t.Helper()
		var n int
		if err := p.Pool.QueryRow(ctx, `SELECT count(*) FROM (`+latest+`) t WHERE t.external_ref LIKE $5`,
			sa.triage(f.run+"-%")...).Scan(&n); err != nil {
			t.Fatalf("count triages: %v", err)
		}
		return n
	}
	// Restricted to this run's hosts (every fixture fault host carries the run id) so a perturbed window
	// cannot be inflated by another fixture's rows — only by this one's edge rows.
	countFaults := func(a scopeArgs, pred string) int {
		t.Helper()
		var n int
		if err := p.Pool.QueryRow(ctx, `SELECT count(*) FROM injected_fault f WHERE `+pred+` AND f.host LIKE $5`,
			a.faults("%"+f.run+"%")...).Scan(&n); err != nil {
			t.Fatalf("count faults: %v", err)
		}
		return n
	}
	estate, err := NewAxisReadStore(p).resolveScope(ctx, AxisScope{Since: f.base, Until: f.until})
	if err != nil {
		t.Fatalf("resolve estate: %v", err)
	}
	perturb := func(src, arm string) string {
		t.Helper()
		if !strings.Contains(src, arm) {
			t.Fatalf("the shipped predicate no longer contains %q — this control would pass vacuously", arm)
		}
		return strings.Replace(src, arm, "("+arm+" OR true)", 1)
	}

	if got := countTriages(latestInScope); got != 2 {
		t.Fatalf("baseline dc1 incidents = %d, want 2", got)
	}
	// Site arm gone -> every in-window incident of the run is dc1's: 5.
	if got := countTriages(perturb(latestInScope, "sa.site = ANY($4::text[])")); got <= 2 {
		t.Errorf("dropping the site arm changed nothing (%d) — the fixture cannot see a site leak", got)
	}
	// Upper bound gone -> nlLate (AT until) leaks in: 3.
	if got := countTriages(perturb(latestInScope, "created_at < $2")); got != 3 {
		t.Errorf("dropping the exclusive upper bound gave %d, want 3 (nlLate sits exactly at until)", got)
	}
	// Lower bound gone -> nlEarly leaks in: 3.
	if got := countTriages(perturb(latestInScope, "created_at >= $1")); got != 3 {
		t.Errorf("dropping the lower bound gave %d, want 3 (nlEarly sits a minute before since)", got)
	}
	if got := countFaults(sa, faultInScope); got != 1 {
		t.Fatalf("baseline dc1 faults = %d, want 1", got)
	}
	// Site arm gone -> all five in-window faults, GR and unattributed included.
	if got := countFaults(sa, perturb(faultInScope, "f.id = ANY($4::bigint[])")); got != 5 {
		t.Errorf("dropping the fault-site arm gave %d, want 5 — GR and unattributed faults would score as NL", got)
	}
	// The window bounds are what an ESTATE fault scope rests on (a site scope's ids are already windowed).
	if got := countFaults(estate, faultInScope); got != 5 {
		t.Fatalf("baseline estate faults = %d, want 5", got)
	}
	if got := countFaults(estate, perturb(faultInScope, "f.injected_at < $2")); got != 6 {
		t.Errorf("dropping the fault window's exclusive upper bound gave %d, want 6 (one fault sits AT until)", got)
	}
	if got := countFaults(estate, perturb(faultInScope, "f.injected_at >= $1")); got != 6 {
		t.Errorf("dropping the fault window's lower bound gave %d, want 6 (one fault sits before since)", got)
	}
}

// faultSite is pure; its refusal to guess is the property that keeps a mislabelled fault out of BOTH sites.
func TestFaultSite(t *testing.T) {
	for _, c := range []struct{ host, node, want string }{
		{"dc1tg632x", "", "dc1"},
		{"dc2tg632x", "", "dc2"},
		{"tg632x", "dc2pvex", "dc2"},        // node only
		{"tg632x", "dc1pvex", "dc1"},        // node only
		{"dc2tg632x", "dc2pvex", "dc2"}, // agree
		{"dc2tg632x", "dc1pvex", ""},       // disagree: refuse
		{"tg632x", "", ""},                         // nothing
		{"tg632x", "tg632node", ""},                // neither declared
	} {
		if got := faultSite(c.host, c.node); got != c.want {
			t.Errorf("faultSite(%q, %q) = %q, want %q", c.host, c.node, got, c.want)
		}
	}
}

// deterministicHealTier must be the runner's spelling, or the deterministic lane silently re-enters the harness
// check (and vanishes from its own line). core/db cannot import temporal/runner, so read its source.
func TestDeterministicHealTierMatchesTheRunner(t *testing.T) {
	src, err := os.ReadFile("../../temporal/runner/deterministic_heal.go")
	if err != nil {
		t.Fatalf("read the runner's declaration: %v", err)
	}
	if !strings.Contains(string(src), `deterministicHealTier = "`+deterministicHealTier+`"`) {
		t.Errorf("temporal/runner no longer declares deterministicHealTier = %q — core/db's copy has drifted", deterministicHealTier)
	}
}
