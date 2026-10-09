package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// THE TRACE WALK MUST NOT RENDER ANOTHER INCIDENT'S ROWS AS THIS INCIDENT'S.
//
// action_id is the hash of the action SHAPE (INV-07), so it is the same value on every incident that ever
// proposes that shape. Three reads in the spine walk keyed on it alone:
//
//	interceptor_gate_verdict — fixed in !685; live: 503 rows / 47 incidents rendered as one "170-gate chain"
//	policy_decision          — fixed here; live 2026-07-29: 17 of 30 sessions showed a decision predating
//	                           their own proposal, one a full day earlier
//	action_verdict           — fixed here; live 2026-07-29: 22 of 30, three sessions all showing the SAME
//	                           `match` stamped 2026-07-23T02:20:23, six days earlier
//
// This drives the REAL pgx reader against a REAL database, which is the only way to catch it: an in-memory
// fake returns whatever the fake was told to return, and every one of these defects lived in the SQL.
//
// SHAPE OF THE TEST: write ONE action shape, executed by incident A, then have incident B propose the SAME
// shape and never execute it. B must inherit nothing from A.
//
// Gated on TG_TEST_POSTGRES_DSN (CI provides it; see .gitlab-ci.yml).
func TestTraceWalkIsScopedToOneIncident(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the incident-scope test")
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

	uniq := fmt.Sprintf("scope-it-%d", os.Getpid())
	actionID := uniq + "-shared-action" // ONE shape, TWO incidents — the whole point
	refA, refB, refC := uniq+"-incident-a", uniq+"-incident-b", uniq+"-incident-c"
	planHash := uniq + "-plan"

	// A executed six days ago; B proposed the same shape today and never executed.
	tA := time.Date(2026, 7, 23, 2, 20, 23, 0, time.UTC)
	tB := time.Date(2026, 7, 29, 0, 9, 31, 0, time.UTC)

	defer func() {
		for _, q := range []string{
			`DELETE FROM action_verdict WHERE action_id = $1`,
			`DELETE FROM action_execution WHERE action_id = $1`,
			`DELETE FROM policy_decision WHERE action_id = $1`,
			`DELETE FROM interceptor_gate_verdict WHERE action_id = $1`,
		} {
			if _, err := p.Exec(ctx, q, actionID); err != nil {
				t.Errorf("cleanup %q: %v", q, err) // checked: a swallowed cleanup error hides a wrong table/column
			}
		}
		for _, ref := range []string{refA, refB, refC} {
			for _, q := range []string{
				`DELETE FROM session_triage WHERE external_ref = $1`,
				`DELETE FROM session_risk_audit WHERE external_ref = $1`,
			} {
				if _, err := p.Exec(ctx, q, ref); err != nil {
					t.Errorf("cleanup %s %s: %v", ref, q, err)
				}
			}
		}
	}()

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := p.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	// --- incident A: classified, proposed, executed, verified (six days ago) ---
	// The classification row is what carries action_id into the walk — without it the whole execute-path block
	// is skipped and every assertion below passes vacuously.
	mustExec(`INSERT INTO session_risk_audit (external_ref, risk_level, band, action_id, schema_version, created_at)
	          VALUES ($1,'low','AUTO',$2,1,$3)`, refA, actionID, tA)
	mustExec(`INSERT INTO session_triage (external_ref, host, alert_rule, band, outcome, proposed, op, conclusion, confidence, created_at)
	          VALUES ($1,'dc1mealie01','device-down','AUTO','executed',true,'start-guest','a',0.9,$2)`, refA, tA)
	mustExec(`INSERT INTO interceptor_gate_verdict (action_id, external_ref, ordinal, gate, verdict, reason, created_at)
	          VALUES ($1,$2,1,'execute','pass','ok',$3)`, actionID, refA, tA)
	// the verify gate lands AFTER the verdict, as it does in production — see TestVerdictSurvivesAGateWrittenAfterIt
	mustExec(`INSERT INTO interceptor_gate_verdict (action_id, external_ref, ordinal, gate, verdict, reason, created_at)
	          VALUES ($1,$2,2,'verify','pass','ok',$3)`, actionID, refA, tA.Add(2*time.Second))
	mustExec(`INSERT INTO action_verdict (action_id, plan_hash, verdict, target_host, site, schema_version, created_at)
	          VALUES ($1,$2,'match','dc1mealie01','dc1',1,$3)`, actionID, planHash, tA)
	mustExec(`INSERT INTO policy_decision (action_id, external_ref, verdict, composed_band, bundle_version, matched_rules, reason, mode, created_at)
	          VALUES ($1,$2,'auto','AUTO','bundle-A','[]'::jsonb,'decided for incident A','Semi-auto',$3)`, actionID, refA, tA)

	// --- incident B: classified on the SAME shape today, never executed, never verified ---
	mustExec(`INSERT INTO session_risk_audit (external_ref, risk_level, band, action_id, schema_version, created_at)
	          VALUES ($1,'low','POLL_PAUSE',$2,1,$3)`, refB, actionID, tB)
	mustExec(`INSERT INTO session_triage (external_ref, host, alert_rule, band, outcome, proposed, op, conclusion, confidence, created_at)
	          VALUES ($1,'dc1mealie01','device-down','AUTO','proposed',true,'start-guest','b',0.9,$2)`, refB, tB)
	mustExec(`INSERT INTO policy_decision (action_id, external_ref, verdict, composed_band, bundle_version, matched_rules, reason, mode, created_at)
	          VALUES ($1,$2,'approve','POLL_PAUSE','bundle-B','[]'::jsonb,'decided for incident B','Semi-auto',$3)`, actionID, refB, tB)

	store := &TraceSpineStore{p: p}

	recB, err := store.Load(ctx, refB)
	if err != nil {
		t.Fatalf("load B: %v", err)
	}

	// 1. THE VERDICT. B never executed, so it has no verdict of its own. The shared row belongs to A.
	if recB.Verdict.Present {
		t.Errorf("incident B rendered a mechanical verdict %q stamped %s — that row was written for incident A "+
			"six days earlier. A verdict is the PROOF a heal did what it claimed; a borrowed one is a claim of "+
			"proof. Live 2026-07-29: 22 of 30 consecutive sessions did exactly this",
			recB.Verdict.Verdict, recB.Verdict.CreatedAt.Format(time.RFC3339))
	}

	// 2. THE POLICY DECISION. Both incidents decided the same shape; B must read ITS OWN authorization.
	if !recB.Policy.Present {
		t.Fatalf("incident B lost its own policy decision — the ref predicate must not narrow away a row that " +
			"genuinely belongs to this incident")
	}
	if recB.Policy.BundleVersion != "bundle-B" || recB.Policy.Verdict != "approve" {
		t.Errorf("incident B read policy verdict=%q bundle=%q, want approve/bundle-B — it is showing incident "+
			"A's authorization (an auditor reads this first to answer 'who allowed this?')",
			recB.Policy.Verdict, recB.Policy.BundleVersion)
	}

	// 3. THE GATE CHAIN. A's execute gate must not appear in B's walk.
	for _, g := range recB.GateVerdicts {
		t.Errorf("incident B inherited gate %q/%q stamped %s from incident A", g.Gate, g.Verdict,
			g.CreatedAt.Format(time.RFC3339))
	}

	// --- incident C: SAME shape, executed TODAY, and it went WRONG ---
	// The decisive case. A and C ran the same action and got DIFFERENT outcomes. Only a per-execution row can
	// express that; action_verdict physically cannot, since A already claimed its single first-wins row.
	tC := tB.Add(time.Minute)
	mustExec(`INSERT INTO session_risk_audit (external_ref, risk_level, band, action_id, schema_version, created_at)
	          VALUES ($1,'low','AUTO',$2,1,$3)`, refC, actionID, tC)
	mustExec(`INSERT INTO interceptor_gate_verdict (action_id, external_ref, ordinal, gate, verdict, reason, created_at)
	          VALUES ($1,$2,1,'execute','pass','ok',$3)`, actionID, refC, tC)
	mustExec(`INSERT INTO action_execution (action_id, external_ref, verdict, unverifiable, target_host, site, schema_version, executed_at)
	          VALUES ($1,$2,'deviation',false,'dc1mealie01','dc1',1,$3)`, actionID, refC, tC)

	recC, err := store.Load(ctx, refC)
	if err != nil {
		t.Fatalf("load C: %v", err)
	}
	if !recC.Verdict.Present {
		t.Errorf("incident C has its OWN action_execution row and the walk showed no verdict — the per-execution "+
			"table (migration 0043) exists precisely to answer this and nothing was reading it")
	} else if recC.Verdict.Verdict != "deviation" {
		t.Errorf("incident C read verdict %q, want deviation — it is showing incident A's `match`. A and C ran "+
			"the same action shape and got DIFFERENT outcomes; reporting A's for both is how a failed heal "+
			"disappears behind an earlier success", recC.Verdict.Verdict)
	}

	// 4. AND THE CONVERSE — the narrowing must not blind the incident that DID execute. Without this, deleting
	//    every row would pass assertions 1-3, which is the vacuous-oracle failure mode.
	recA, err := store.Load(ctx, refA)
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	if !recA.Verdict.Present || recA.Verdict.Verdict != "match" {
		t.Errorf("incident A lost its OWN verdict (present=%v verdict=%q) — the scope check must reject foreign "+
			"rows, not all rows", recA.Verdict.Present, recA.Verdict.Verdict)
	}
	if recA.Policy.BundleVersion != "bundle-A" {
		t.Errorf("incident A read bundle %q, want bundle-A", recA.Policy.BundleVersion)
	}
	if len(recA.GateVerdicts) != 2 {
		t.Errorf("incident A has %d gate rows, want its own 2", len(recA.GateVerdicts))
	}
}

// THE ANCHOR MUST NOT BE THE END OF THE GATE CHAIN. Pinned explicitly because I shipped this bug into review:
// the first version anchored on the LATEST gate row, on the reasoning that a genuine verdict is written after
// the action executes. True — but the chain does not end at `execute`, it ends at `verify`, emitted AFTER the
// verifier writes action_verdict (live: execute 00:19:30, verify 00:19:31 on librenms-dc1-181284). So the
// anchor overshot the verdict and threw away real ones.
//
// Two existing tests caught it, which is luck, not coverage: both only run when TG_TEST_POSTGRES_DSN is set,
// and neither says anything about anchors, so a future refactor could satisfy them while reintroducing this.
// The layout below is the one that bites — a verdict stamped BETWEEN the execute gate and the verify gate,
// which is exactly where a real one lands.
func TestVerdictSurvivesAGateWrittenAfterIt(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the anchor-overshoot test")
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

	uniq := fmt.Sprintf("anchor-it-%d", os.Getpid())
	actionID, ref, planHash := uniq+"-act", uniq+"-ref", uniq+"-plan"
	t0 := time.Date(2026, 7, 29, 0, 19, 0, 0, time.UTC)

	defer func() {
		for _, q := range []string{
			`DELETE FROM action_verdict WHERE action_id = $1`,
			`DELETE FROM action_execution WHERE action_id = $1`,
			`DELETE FROM interceptor_gate_verdict WHERE action_id = $1`,
		} {
			if _, err := p.Exec(ctx, q, actionID); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
		if _, err := p.Exec(ctx, `DELETE FROM session_risk_audit WHERE external_ref = $1`, ref); err != nil {
			t.Errorf("cleanup audit: %v", err)
		}
	}()

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := p.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	mustExec(`INSERT INTO session_risk_audit (external_ref, risk_level, band, action_id, schema_version, created_at)
	          VALUES ($1,'low','AUTO',$2,1,$3)`, ref, actionID, t0)
	mustExec(`INSERT INTO interceptor_gate_verdict (action_id, external_ref, ordinal, gate, verdict, reason, created_at)
	          VALUES ($1,$2,11,'execute','pass','ok',$3)`, actionID, ref, t0.Add(30*time.Second))
	// the verdict lands between the two gates, as it does in production
	mustExec(`INSERT INTO action_verdict (action_id, plan_hash, verdict, target_host, site, schema_version, created_at)
	          VALUES ($1,$2,'match','dc1mealie01','dc1',1,$3)`, actionID, planHash, t0.Add(31*time.Second))
	mustExec(`INSERT INTO interceptor_gate_verdict (action_id, external_ref, ordinal, gate, verdict, reason, created_at)
	          VALUES ($1,$2,12,'verify','pass','ok',$3)`, actionID, ref, t0.Add(32*time.Second))

	rec, err := (&TraceSpineStore{p: p}).Load(ctx, ref)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !rec.Verdict.Present || rec.Verdict.Verdict != "match" {
		t.Errorf("this incident's OWN verdict was rejected (present=%v verdict=%q) — the anchor overshot it. "+
			"An anchor derived from the END of the gate chain lands after the verifier has already written, so "+
			"it discards exactly the rows it exists to protect", rec.Verdict.Present, rec.Verdict.Verdict)
	}
}
