package db

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/knowledge"
)

// GOLDEN-FIXTURE TESTS FOR THE MEASUREMENT SQL (spec/025 REQ-2501).
//
// This file exists because core/db/axis_read.go computes every published benchmark axis — the numbers behind
// the v1.0 claim — and had NO test at all. Three measurement defects shipped through that gap in a single
// week: a verified-match rate that pooled executed actions with never-executed predictions, a head-to-head
// overall that pooled a dimension one side never competes on, and an MTTR derived from a join that produced
// durations seven days negative. Each was a JOIN-semantics defect, which is exactly the class a fake cannot
// reproduce.
//
// So these run against a REAL PostgreSQL with every migration applied, gated on TG_TEST_DSN. A pgx fake has
// already hidden a field-drop in this repository once; for measurement SQL a stub is worse than nothing,
// because it produces confident green.
//
// EXPECTED VALUES ARE HAND-COMPUTED from the fixture, never captured from the implementation's output — a
// wrong implementation must not be able to bless itself. Each fixture is small enough to verify by eye.
//
// THE MUTATION CONTROL (TestAxisRead_MutationControl) is the load-bearing half: it perturbs one predicate of
// an axis query and asserts the golden expectation goes RED. A test that stays green under a deliberately
// broken query proves nothing — this project has already shipped a CI gate that ran on every commit while
// examining nothing.

// testDSN returns the DSN for the golden fixture, or "" when the suite should skip.
func testDSN() string { return os.Getenv("TG_TEST_DSN") }

func skipWithoutDB(t *testing.T) string {
	t.Helper()
	dsn := testDSN()
	if dsn == "" {
		t.Skip("TG_TEST_DSN not set — the golden axis fixture needs a real Postgres with all migrations " +
			"applied (CI supplies one; see the harness job). Skipping locally is a convenience, never the CI path.")
	}
	return dsn
}

// axisFixture seeds a small, hand-verifiable corpus and returns a cleanup.
//
// The shape is chosen so every assertion below can be checked by reading this function:
//   - 4 triages, of which 3 proposed and 2 mutated
//   - 1 mutated incident confirmed clear  ⇒ A3 = 1/2
//   - 1 mutated incident on a suspicious actor ⇒ A7 numerator = 1
//   - 2 mutated incidents, of which ONE has a correlated recovery 120s later ⇒ A6b n=1, median 120
//     (the other's recovery is on a DIFFERENT host, so it must NOT correlate — that is the A6b defect)
func axisFixture(ctx context.Context, t *testing.T, p *Pool) func() {
	t.Helper()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	const pfx = "gold-axis-"

	cleanup := func() {
		_, _ = p.Exec(ctx, `DELETE FROM ingest_transition WHERE external_ref LIKE $1`, pfx+"%")
		_, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref LIKE $1`, pfx+"%")
		_, _ = p.Exec(ctx, `DELETE FROM session_risk_audit WHERE external_ref LIKE $1`, pfx+"%")
		_, _ = p.Exec(ctx, `DELETE FROM injected_fault WHERE host LIKE $1`, "goldhost-%")
	}
	cleanup()

	rows := []struct {
		ref        string
		host, rule string
		mutated    bool
		clear      bool
		attrib     string
		offset     time.Duration
	}{
		{pfx + "1", "goldhost-a", "Device-Down", true, true, "authorized-test", 0},
		{pfx + "2", "goldhost-b", "Device-Down", true, false, "attributed-suspicious", time.Minute},
		{pfx + "3", "goldhost-c", "Space-on-/", false, false, "", 2 * time.Minute},
		{pfx + "4", "goldhost-d", "Space-on-/", false, false, "", 3 * time.Minute},
	}
	for _, r := range rows {
		if _, err := p.Exec(ctx, `
			INSERT INTO session_triage (external_ref, host, alert_rule, band, op, op_class, proposed,
			                            predicted, outcome, conclusion, step_count, mutated, confirmed_clear,
			                            actor_attribution, created_at)
			VALUES ($1,$2,$3,'AUTO','start','start-guest',true,true,'ok','c',3,$4,$5,$6,$7)`,
			r.ref, r.host, r.rule, r.mutated, r.clear, r.attrib, base.Add(r.offset)); err != nil {
			t.Fatalf("seed triage %s: %v", r.ref, err)
		}
	}

	// Recovery for incident 1 — same host AND rule, 120s after its triage ⇒ MUST correlate.
	if _, err := p.Exec(ctx, `
		INSERT INTO ingest_transition (external_ref, kind, host, site, alert_rule, observed_at, received_at)
		VALUES ($1,'recovery','goldhost-a','dc1','Device-Down',$2,$2)`,
		pfx+"rec-1", base.Add(120*time.Second)); err != nil {
		t.Fatalf("seed recovery 1: %v", err)
	}
	// A recovery on a DIFFERENT host — must NOT be attributed to incident 2. This is the exact defect the
	// correlation bound exists to prevent, so the fixture must contain a decoy.
	if _, err := p.Exec(ctx, `
		INSERT INTO ingest_transition (external_ref, kind, host, site, alert_rule, observed_at, received_at)
		VALUES ($1,'recovery','goldhost-ZZZ','dc1','Device-Down',$2,$2)`,
		pfx+"rec-decoy", base.Add(130*time.Second)); err != nil {
		t.Fatalf("seed decoy recovery: %v", err)
	}
	// A4 COMPOSITION FIXTURE. Three POLL_PAUSE risk-audit rows: two attribution escalations and one novel
	// incident. Exactly ONE of the escalations sits on a host carrying an injected fault that is still open, so
	// the harness-artefact count is hand-computed as 1 — never 2, never 3.
	for _, r := range []struct {
		ref, reason string
	}{
		{pfx + "esc-injected", "actor-attribution-escalate"},
		{pfx + "esc-organic", "actor-attribution-escalate"},
		{pfx + "novel", "ood-novel-incident"},
	} {
		if _, err := p.Exec(ctx, `
			INSERT INTO session_risk_audit (external_ref, risk_level, band, signals_json, action_id, schema_version, created_at)
			VALUES ($1,'high','POLL_PAUSE',jsonb_build_object('poll_reason',$2::text),$3,1,$4)`,
			r.ref, r.reason, r.ref+"-act", base.Add(90*time.Second)); err != nil {
			t.Fatalf("seed risk audit %s: %v", r.ref, err)
		}
		// The join to injected_fault goes through session_triage.host, so each audit row needs its triage row.
		if _, err := p.Exec(ctx, `
			INSERT INTO session_triage (external_ref, host, alert_rule, band, proposed, mutated, created_at)
			VALUES ($1,$2,'Device-Down','POLL_PAUSE',true,false,$3)`,
			r.ref, "goldhost-"+r.ref, base.Add(90*time.Second)); err != nil {
			t.Fatalf("seed triage for %s: %v", r.ref, err)
		}
	}
	// ONLY the first escalation's host carries a fault, and it is still outstanding at the escalation time.
	if _, err := p.Exec(ctx, `
		INSERT INTO injected_fault (host, fault_type, note, restore_state, restore_due_at, fault_ref, node, injected_at)
		VALUES ($1,'device-down','gold axis fixture','pending',$2,'100','goldnode',$3)`,
		"goldhost-"+pfx+"esc-injected", base.Add(30*time.Minute), base.Add(60*time.Second)); err != nil {
		t.Fatalf("seed injected fault: %v", err)
	}

	return cleanup
}

// A4 COMPOSITION. "POLL_PAUSE=n" alone cannot be interpreted: it does not say what the human was asked. This
// is the axis's population under REQ-2502, and the harness-artefact share is the reason a published A4 needs a
// caveat rather than a footnote. Hand-computed from the fixture: 2 escalations, 1 novel, 1 of the escalations
// on an injected fault.
func TestAxisRead_A4PollReasonComposition_GoldenFixture(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()

	agg, err := NewAxisReadStore(p).Aggregate(ctx, time.Now().UTC().Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if got := agg.PollReasons["actor-attribution-escalate"]; got < 2 {
		t.Errorf("PollReasons[actor-attribution-escalate] = %d, want >= 2 — without the composition A4 reports "+
			"a bare POLL_PAUSE count and nobody can tell what the human was asked, so the axis cannot be "+
			"interpreted or improved", got)
	}
	if got := agg.PollReasons["ood-novel-incident"]; got < 1 {
		t.Errorf("PollReasons[ood-novel-incident] = %d, want >= 1 — the map must carry every reason, not just "+
			"the dominant one", got)
	}
	if agg.AttribEscalOnInjected < 1 {
		t.Errorf("AttribEscalOnInjected = %d, want >= 1 — the harness-artefact share is the whole point: a "+
			"synthetic fault has no provenance to attribute, so it escalates and depresses A4 on this estate",
			agg.AttribEscalOnInjected)
	}
}

// MUTATION CONTROL for the artefact share: the injected-fault containment test is load-bearing. Without the
// EXISTS predicate every escalation would count as an artefact, turning a measured caveat into a blanket
// excuse for the axis — which is exactly the direction that flatters a number.
func TestAxisRead_MutationControl_ArtefactShareCountsOnlyInjectedHosts(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()

	agg, err := NewAxisReadStore(p).Aggregate(ctx, time.Now().UTC().Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	total := agg.PollReasons["actor-attribution-escalate"]
	if agg.AttribEscalOnInjected >= total {
		t.Errorf("AttribEscalOnInjected = %d of %d escalations — the fixture seeds exactly ONE escalation on an "+
			"injected host and one on a clean host, so counting all of them means the containment predicate is "+
			"gone and every escalation would be written off as a harness artefact", agg.AttribEscalOnInjected, total)
	}
}

func openFixture(t *testing.T) (context.Context, *Pool, func()) {
	t.Helper()
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	clean := axisFixture(ctx, t, p)
	return ctx, p, func() { clean(); p.Close() }
}

// A3 (heal success) and A7 (false actuation) share the mutated denominator. Hand-computed from the fixture:
// 2 mutated, 1 confirmed clear, 1 on a suspicious actor.
func TestAxisRead_HealAndFalseActuation_GoldenFixture(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()

	agg, err := NewAxisReadStore(p).Aggregate(ctx, time.Now().UTC().Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if agg.MutatedCount < 2 {
		t.Fatalf("MutatedCount = %d, want >= 2 (the fixture seeds exactly 2)", agg.MutatedCount)
	}
	if agg.HealConfirmedCount < 1 {
		t.Fatalf("HealConfirmedCount = %d, want >= 1", agg.HealConfirmedCount)
	}
	if agg.SuspiciousActuations < 1 {
		t.Fatalf("SuspiciousActuations = %d, want >= 1 — a mutation on an 'attributed-suspicious' incident "+
			"is the A7 numerator and must not be silently dropped", agg.SuspiciousActuations)
	}
}

// A6b (time to recovery). Hand-computed: of 2 mutated incidents exactly ONE has a same-host, same-rule
// recovery, 120s after its triage. The decoy recovery is on another host and must be ignored.
func TestAxisRead_TimeToRecovery_GoldenFixture(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()

	agg, err := NewAxisReadStore(p).Aggregate(ctx, time.Now().UTC().Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if agg.HealCorrelatedCount < 1 {
		t.Fatal("HealCorrelatedCount = 0 — the same-host/same-rule recovery 120s after the triage did not " +
			"correlate; A6b would silently report nothing")
	}
	if agg.HealMedianSec <= 0 {
		t.Fatalf("HealMedianSec = %d, want > 0 — a non-positive duration is the exact defect A6b replaced "+
			"(the old join produced durations seven days negative)", agg.HealMedianSec)
	}
	if agg.HealCorrelatedCount > agg.MutatedCount {
		t.Fatalf("HealCorrelatedCount (%d) exceeds MutatedCount (%d) — the decoy recovery on a different "+
			"host was wrongly attributed, inflating the denominator", agg.HealCorrelatedCount, agg.MutatedCount)
	}
}

// THE MUTATION CONTROL (REQ-2501). Perturbing one predicate of the A6b correlation — dropping the host
// match, which is what makes the correlation sound — must change the result. If it does not, the golden
// test above is tracking the query rather than constraining it, and provides no protection.
func TestAxisRead_MutationControl_HostPredicateIsLoadBearing(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()

	// PERTURB THE SHIPPED TEXT, not a copy of it. The previous version of this control wrote out its own
	// `correct` SQL, dropped `x.host = t.host` from that, and compared the two counts — which demonstrates only
	// that SQL means what SQL means. It never called the implementation, so if the SHIPPED query had lost its
	// host predicate this control would still have passed. It guarded nothing.
	//
	// healCorrelationMatch is now the constant the implementation interpolates, so perturbing it here perturbs
	// the real correlation.
	if !strings.Contains(healCorrelationMatch, "x.host = t.host") {
		t.Fatal("the shipped correlation predicate has LOST its host match — a recovery on any host would be " +
			"attributed to this incident, inflating HealCorrelatedCount and corrupting the percentiles")
	}
	perturbed := strings.Replace(healCorrelationMatch, "AND x.host = t.host\n", "", 1)
	if perturbed == healCorrelationMatch {
		t.Fatal("the perturbation did not change the predicate — this control would pass vacuously")
	}

	// The predicate folds rule names through the family table, so the harness must materialize the same
	// CTE and joins the implementation does. Reproducing the SHAPE is unavoidable here; what matters is
	// that the PREDICATE TEXT is the shipped constant, which is the thing the control exists to perturb.
	aliases, canons := knowledge.RuleFamilyPairs()
	count := func(pred string) int {
		var n int
		q := `WITH fam(alias, canon) AS (SELECT * FROM unnest($1::text[], $2::text[]))
		      SELECT count(*) FROM session_triage t
		        LEFT JOIN fam ft ON ft.alias = lower(btrim(t.alert_rule))
		       WHERE t.mutated AND t.external_ref LIKE 'gold-axis-%'
		        AND EXISTS (SELECT 1 FROM ingest_transition x
		                      LEFT JOIN fam fx ON fx.alias = lower(btrim(x.alert_rule))
		                     WHERE ` + pred + `
		                     AND x.observed_at < t.created_at + interval '6 hours')`
		if err := p.Pool.QueryRow(ctx, q, aliases, canons).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	got, loose := count(healCorrelationMatch), count(perturbed)
	if got == loose {
		t.Fatalf("dropping the host match changed nothing (%d both ways) — the fixture does not contain a "+
			"cross-host decoy recovery, so this control proves nothing about the predicate", got)
	}
	if loose <= got {
		t.Fatalf("dropping the host match must ADMIT the decoy recovery (correct=%d loose=%d)", got, loose)
	}
}

// TestAxisHealCorrelationCrossesRuleFamilyAliases pins the fix for a silent exclusion from TG's only
// wall-clock MTTR number.
//
// The A6b correlation matched `x.alert_rule = t.alert_rule` — string equality. modules/ingest/pveliveness
// raises under TG's own label "Device-Down", while captured recovery transitions carry LibreNMS spellings
// ("Devices-up/down", "Device-Down-SNMP-unreachable", "Device-Down-Due-to-no-ICMP-response."). Those two
// vocabularies never intersect, so every incident of that class — the commonest one in this estate —
// correlated to nothing and vanished from the denominator. Not counted as slow: ABSENT. The metric
// therefore looked better the more of this class occurred.
//
// The recovery belt (TransitionLogStore.RecoveredSince) was fixed for exactly this on 2026-07-30 and this
// query was not, so the two answered different questions about the same pair of rows.
//
// WHY THE EXISTING FIXTURE COULD NOT CATCH IT: TestAxisRead_TimeToRecovery seeds the triage as
// "Device-Down" and its recovery as "Device-Down" — same spelling on both sides. Equality and family
// folding agree on that row, so it passes under either implementation. The bug lives exactly in the gap
// the fixture had no row for.
//
// KILLING MUTATION: restore `AND x.alert_rule = t.alert_rule` (and drop the COALESCE clause) in
// core/db/axis_read.go. RED.
func TestAxisHealCorrelationCrossesRuleFamilyAliases(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()

	const pfx = "gold-fam-"
	base := time.Now().UTC().Add(-2 * time.Hour)
	cleanup := func() {
		_, _ = p.Pool.Exec(ctx, `DELETE FROM ingest_transition WHERE external_ref LIKE $1`, pfx+"%")
		_, _ = p.Pool.Exec(ctx, `DELETE FROM session_triage WHERE external_ref LIKE $1`, pfx+"%")
	}
	cleanup()
	defer cleanup()

	// The triage arrives under TG's own pveliveness label...
	if _, err := p.Pool.Exec(ctx, `
		INSERT INTO session_triage (external_ref, host, alert_rule, band, op, op_class, proposed, predicted,
		                            outcome, conclusion, step_count, mutated, confirmed_clear,
		                            actor_attribution, created_at)
		VALUES ($1,'famhost-a','Device-Down','AUTO','start','start-guest',true,true,'ok','c',3,true,true,
		        'authorized-test',$2)`, pfx+"1", base); err != nil {
		t.Fatalf("seed triage: %v", err)
	}
	// ...and its recovery under a LibreNMS sibling spelling. Same condition, different vocabulary.
	if _, err := p.Pool.Exec(ctx, `
		INSERT INTO ingest_transition (external_ref, kind, host, site, alert_rule, observed_at, received_at)
		VALUES ($1,'recovery','famhost-a','dc1','Devices-up/down',$2,$2)`,
		pfx+"rec", base.Add(90*time.Second)); err != nil {
		t.Fatalf("seed recovery: %v", err)
	}

	// Guard the fixture itself: if these two ever stop being family siblings, this test would silently
	// stop testing anything.
	if knowledge.CanonicalRule("Device-Down") != knowledge.CanonicalRule("Devices-up/down") {
		t.Fatal("fixture precondition lost: Device-Down and Devices-up/down are no longer one family, so " +
			"this test no longer exercises cross-vocabulary correlation")
	}

	aliases, canons := knowledge.RuleFamilyPairs()
	if len(aliases) == 0 {
		t.Fatal("vacuity floor: the family table is EMPTY, so folding degenerates to equality and a pass " +
			"here would certify nothing")
	}

	var n int
	q := `WITH fam(alias, canon) AS (SELECT * FROM unnest($1::text[], $2::text[]))
	      SELECT count(*) FROM session_triage t
	        LEFT JOIN fam ft ON ft.alias = lower(btrim(t.alert_rule))
	       WHERE t.mutated AND t.external_ref LIKE 'gold-fam-%'
	        AND EXISTS (SELECT 1 FROM ingest_transition x
	                      LEFT JOIN fam fx ON fx.alias = lower(btrim(x.alert_rule))
	                     WHERE ` + healCorrelationMatch + `
	                     AND x.observed_at < t.created_at + interval '6 hours')`
	if err := p.Pool.QueryRow(ctx, q, aliases, canons).Scan(&n); err != nil {
		t.Fatalf("correlate: %v", err)
	}
	if n != 1 {
		t.Fatalf("a pveliveness \"Device-Down\" incident recovered under the LibreNMS sibling "+
			"\"Devices-up/down\" did NOT correlate (got %d, want 1) — it is silently absent from the MTTR "+
			"denominator, which makes the metric look better the more of this class occurs", n)
	}
}

// The narrowness is the other half: folding must not let an UNRELATED rule's recovery confirm an
// incident. rulefamily.json deliberately excludes TargetDown (a scrape target down while the host is up).
func TestAxisHealCorrelationStillRejectsAnUnrelatedRule(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()

	const pfx = "gold-fam2-"
	base := time.Now().UTC().Add(-2 * time.Hour)
	cleanup := func() {
		_, _ = p.Pool.Exec(ctx, `DELETE FROM ingest_transition WHERE external_ref LIKE $1`, pfx+"%")
		_, _ = p.Pool.Exec(ctx, `DELETE FROM session_triage WHERE external_ref LIKE $1`, pfx+"%")
	}
	cleanup()
	defer cleanup()

	if _, err := p.Pool.Exec(ctx, `
		INSERT INTO session_triage (external_ref, host, alert_rule, band, op, op_class, proposed, predicted,
		                            outcome, conclusion, step_count, mutated, confirmed_clear,
		                            actor_attribution, created_at)
		VALUES ($1,'famhost-b','Device-Down','AUTO','start','start-guest',true,true,'ok','c',3,true,true,
		        'authorized-test',$2)`, pfx+"1", base); err != nil {
		t.Fatalf("seed triage: %v", err)
	}
	// A recovery for a genuinely different condition on the SAME host, inside the window.
	if _, err := p.Pool.Exec(ctx, `
		INSERT INTO ingest_transition (external_ref, kind, host, site, alert_rule, observed_at, received_at)
		VALUES ($1,'recovery','famhost-b','dc1','Space-on-/',$2,$2)`,
		pfx+"rec", base.Add(90*time.Second)); err != nil {
		t.Fatalf("seed recovery: %v", err)
	}
	if knowledge.CanonicalRule("Device-Down") == knowledge.CanonicalRule("Space-on-/") {
		t.Fatal("fixture precondition lost: these two are now one family, so this test cannot detect " +
			"over-broad folding")
	}

	aliases, canons := knowledge.RuleFamilyPairs()
	var n int
	q := `WITH fam(alias, canon) AS (SELECT * FROM unnest($1::text[], $2::text[]))
	      SELECT count(*) FROM session_triage t
	        LEFT JOIN fam ft ON ft.alias = lower(btrim(t.alert_rule))
	       WHERE t.mutated AND t.external_ref LIKE 'gold-fam2-%'
	        AND EXISTS (SELECT 1 FROM ingest_transition x
	                      LEFT JOIN fam fx ON fx.alias = lower(btrim(x.alert_rule))
	                     WHERE ` + healCorrelationMatch + `
	                     AND x.observed_at < t.created_at + interval '6 hours')`
	if err := p.Pool.QueryRow(ctx, q, aliases, canons).Scan(&n); err != nil {
		t.Fatalf("correlate: %v", err)
	}
	if n != 0 {
		t.Fatalf("a disk-space recovery confirmed a device-down incident (got %d, want 0) — folding is too "+
			"broad and is counting heals TG never achieved", n)
	}
}

// A6b TIME TO DECISION (TG-205) — the wall-clock half of the axis, hand-computed from a five-row fixture.
//
// A6 is DEFINED as MTTR ("resolving faster … detection latency, decision latency, actuation path") and every
// implementation measured decision STEPS: a6a_mean_decision_steps here, MeanDecisionSteps in eval/gate. The
// vocabulary and the code had drifted apart, so TG could report how many CYCLES a decision cost and nothing
// about how LONG it took — not even for its own measured ~39s-vs-~11min detection result.
//
// Expected values, computed by hand from the seeded set and never captured from the implementation:
//
//	timed:   4000, 8000, 12000, 40000, 90000 ms   ⇒ n=5
//	median:  percentile_cont(0.5)  over 5 values  ⇒ the 3rd  = 12000 ms
//	p95:     percentile_cont(0.95) over 5 values  ⇒ idx 0.95*(5-1)=3.8 ⇒ 40000 + 0.8*(90000-40000) = 80000 ms
//
// THE EXCLUSION IS THE POINT, AND IT IS WHY THE SHARED FIXTURE'S UNTIMED ROWS ARE LEFT IN PLACE. axisFixture
// seeds seven triages that record no timing at all (decision_ms = 0), exactly like every one of TG's ~537
// historical incidents recorded before migration 0058. Pooling them makes the median 0 — publishing "TG
// decides instantly" for a population that recorded no decision time whatsoever, which is the most flattering
// possible falsehood about this axis and the direction nobody investigates.
//
// KILLING MUTATION (executed 2026-08-04 against a REAL Postgres with all 58 migrations applied): delete
// `WHERE decision_ms > 0` from the time-to-decision query in core/db/axis_read.go. RED with
//
//	DecisionN = 12, want exactly 5 — untimed sessions entered the A6b denominator (7 of them sit in this
//	window). A session that recorded no time did not decide instantly; it did not record
//
// and the median it would then publish is 0 (seven zeros in a twelve-row sample put p50 among them), which is
// the falsehood the filter exists to prevent. Restored ⇒ green. (Dropping decision_ms from the `latest` CTE
// fails earlier still, at the SQL.)
func TestAxisRead_TimeToDecision_GoldenFixture(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()

	const pfx = "gold-dec-"
	base := time.Now().UTC().Add(-30 * time.Minute)
	cleanup := func() { _, _ = p.Pool.Exec(ctx, `DELETE FROM session_triage WHERE external_ref LIKE $1`, pfx+"%") }
	cleanup()
	defer cleanup()

	for i, ms := range []int64{4000, 8000, 12000, 40000, 90000} {
		if _, err := p.Pool.Exec(ctx, `
			INSERT INTO session_triage (external_ref, host, alert_rule, band, op, op_class, proposed, predicted,
			                            outcome, conclusion, step_count, decision_ms, mutated, created_at)
			VALUES ($1,$2,'Device-Down','AUTO','start','start-guest',true,true,'ok','c',3,$3,false,$4)`,
			fmt.Sprintf("%s%d", pfx, i), fmt.Sprintf("dechost-%d", i), ms, base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("seed timed triage %d: %v", i, err)
		}
	}

	// VACUITY FLOOR. The exclusion assertions below are meaningless unless the window actually CONTAINS
	// untimed rows for the query to exclude — a fixture that only ever seeded timed sessions would pass under
	// an implementation with no filter at all.
	var untimed int
	if err := p.Pool.QueryRow(ctx, `SELECT count(*) FROM session_triage
		WHERE created_at >= $1 AND decision_ms = 0`, time.Now().UTC().Add(-2*time.Hour)).Scan(&untimed); err != nil {
		t.Fatalf("count untimed: %v", err)
	}
	if untimed == 0 {
		t.Fatal("VACUITY FLOOR: the window holds no untimed triage, so 'untimed rows are excluded' is not " +
			"being tested — the pre-0058 corpus is exactly this population")
	}

	agg, err := NewAxisReadStore(p).Aggregate(ctx, time.Now().UTC().Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if agg.DecisionN != 5 {
		t.Fatalf("DecisionN = %d, want exactly 5 — untimed sessions entered the A6b denominator (%d of them sit "+
			"in this window). A session that recorded no time did not decide instantly; it did not record",
			agg.DecisionN, untimed)
	}
	if agg.DecisionMedianMs != 12000 {
		t.Fatalf("DecisionMedianMs = %d, want 12000 — the axis now reports that TG reaches its decisions "+
			"instantly, which is what pooling untimed rows produces", agg.DecisionMedianMs)
	}
	if agg.DecisionP95Ms != 80000 {
		t.Fatalf("DecisionP95Ms = %d, want 80000 — percentiles, never a mean: the tail is the half of the "+
			"distribution an operator waits through", agg.DecisionP95Ms)
	}
	// A6a and A6b are different measurements over the same sessions and must not collapse into one another:
	// these rows carry 3 steps each and wildly different durations, which is the whole reason for the split.
	if agg.StepsN == agg.DecisionN && agg.MeanSteps == float64(agg.DecisionMedianMs) {
		t.Fatalf("A6a steps (%v over n=%d) and A6b milliseconds (%d over n=%d) are reading the same value — "+
			"the axis is conflated again", agg.MeanSteps, agg.StepsN, agg.DecisionMedianMs, agg.DecisionN)
	}
}
