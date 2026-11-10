package db

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/schema"
	"github.com/territory-grounder/grounder/core/trace"
	"github.com/territory-grounder/grounder/core/verify"
)

// TestTraceSpineRoundTrip drives the REAL pgx path for the decision-tracer spine reader (spec/020 T-020-10):
// it writes one incident across all four spine tables via the REAL writers, then Loads it back and asserts
// the assembled walk stitches classify → propose → predict → verify by external_ref. It guards the exact
// failure the in-memory fake hides — a SELECT/Scan that drops the external_ref join or a decision-grade field
// (the pgx-fake-hides-field-drop lesson). Gated on TG_TEST_POSTGRES_DSN (CI has no Postgres).
func TestTraceSpineRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the trace spine round-trip test")
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

	uniq := fmt.Sprintf("trace-spine-it-%d", os.Getpid())
	ref, actionID, planHash := uniq+"-ref", uniq+"-act", uniq+"-plan"
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM session_risk_audit WHERE external_ref = $1", ref)
		_, _ = p.Exec(ctx, "DELETE FROM session_triage WHERE external_ref = $1", ref)
		_, _ = p.Exec(ctx, "DELETE FROM infragraph_prediction WHERE external_ref = $1", ref)
		_, _ = p.Exec(ctx, "DELETE FROM action_verdict WHERE action_id = $1", actionID)
		// best-effort: agent_step + interceptor_gate_verdict are append-only (REVOKE DELETE from tg_runtime), so
		// these DELETEs no-op under the runtime role and clean up under the owner role — either way ignored.
		_, _ = p.Exec(ctx, "DELETE FROM agent_step WHERE external_ref = $1", ref)
		_, _ = p.Exec(ctx, "DELETE FROM policy_decision WHERE action_id = $1", actionID)
		_, _ = p.Exec(ctx, "DELETE FROM interceptor_gate_verdict WHERE action_id = $1", actionID)
		_, _ = p.Exec(ctx, "DELETE FROM credential_resolution WHERE external_ref = $1", ref)
	}()

	// classify — the admission row.
	if err := NewRiskAuditStore(p).PersistRiskAudit(audit.RiskAudit{
		ExternalRef: ref, RiskLevel: "low", Band: safety.BandAuto, AutoApproved: true,
		PlanHash: planHash, ActionID: actionID, SchemaVersion: 1,
	}); err != nil {
		t.Fatalf("persist risk audit: %v", err)
	}
	// propose — the parsed proposal + stated confidence (migration 0024).
	if err := NewTriageStore(p).RecordTriage(ctx, judge.TriageRow{
		ExternalRef: ref, Host: "librespeed01", AlertRule: "iface_down", Band: "AUTO",
		Outcome: "proposal", Proposed: true, Op: "restart-service", Conclusion: "flap cleared", Confidence: 0.82,
	}); err != nil {
		t.Fatalf("record triage: %v", err)
	}
	// predict — the committed consequence prediction carrying external_ref (migration 0026, T-020-5).
	if err := NewPredictionStore(p).Commit(ctx, predict.PredictionRecord{
		Prediction: verify.Prediction{
			ActionID: actionID, PlanHash: planHash, TargetHost: "librespeed01", Site: "nl",
			PredictedHosts: map[string]struct{}{"librespeed01": {}},
		},
		ControlHosts:   map[string]struct{}{"web09": {}},
		SchemaVersion:  schema.Version(1),
		PredictionHash: uniq + "-phash",
		ExternalRef:    ref,
	}); err != nil {
		t.Fatalf("commit prediction: %v", err)
	}
	// verify — the mechanical verdict keyed by action_id.
	if err := NewVerdictStore(p).Commit(ctx, actionID, planHash, "librespeed01", "nl", safety.Verdict("match")); err != nil {
		t.Fatalf("commit verdict: %v", err)
	}

	// --- the finer enrichment rows (T-020-8/3/7) the depth-projection reader stitches into the walk ---
	// agent ReAct cycles (agent_step) — cycle-ordered scrubbed investigation.
	for _, c := range []struct {
		cycle                            int
		thought, tool, observation, outc string
	}{
		{1, "check nginx", "get-device-status", "nginx failed", "success"},
		{2, "conclude restart", "", "", "success"},
	} {
		if _, err := p.Exec(ctx, `INSERT INTO agent_step (external_ref, cycle, thought, tool, observation, outcome)
			VALUES ($1,$2,$3,$4,$5,$6)`, ref, c.cycle, c.thought, c.tool, c.observation, c.outc); err != nil {
			t.Fatalf("insert agent_step: %v", err)
		}
	}
	// policy authorization (policy_decision) — verdict + bundle + matched-rule provenance + reason, by action_id.
	if _, err := p.Exec(ctx, `INSERT INTO policy_decision
		(rule_id, verdict, band_mode, composed_band, min_confidence, action_id, plan_hash, principal, mode, bundle_version, matched_rules, reason, external_ref, schema_version)
		VALUES ('rt','auto','respect','AUTO',0,$1,$2,'runner:x','Semi-auto','bundle-rt',$3,'composed auto',$4,1)`,
		actionID, planHash, `[{"id":"rt","verdict":"auto"},{"id":"floor","verdict":"deny"}]`, ref); err != nil {
		t.Fatalf("insert policy_decision: %v", err)
	}
	// interceptor gate chain (interceptor_gate_verdict) — ordinal-ordered, by action_id.
	margin := -0.5 // TG-178: one gate carries a NON-null margin so the read proves it round-trips (not just NULL).
	for _, g := range []struct {
		ord      int
		gate, vd string
		margin   *float64
	}{{1, "admission", "pass", nil}, {2, "mode-chokepoint", "pass", &margin}, {3, "execute", "pass", nil}} {
		if _, err := p.Exec(ctx, `INSERT INTO interceptor_gate_verdict (action_id, external_ref, ordinal, gate, verdict, reason, margin)
			VALUES ($1,$2,$3,$4,$5,'',$6)`, actionID, ref, g.ord, g.gate, g.vd, g.margin); err != nil {
			t.Fatalf("insert interceptor_gate_verdict: %v", err)
		}
	}
	// A DIFFERENT INCIDENT OF THE SAME ACTION — the shape that broke the live console. action_id is
	// CONTENT-ADDRESSED, so every incident that ever proposes this action shape carries the same id. Keyed on
	// action_id alone the gate read returned all of them: measured 2026-07-29, one action_id held 503 rows
	// across 47 incidents over six days, rendered as ONE chain — "170-gate chain", sixteen copies of each
	// gate, three days of timestamps interleaved, and `policy: refuse` sitting among `policy: pass`, which
	// reads as a single decision contradicting itself. It was 47 decisions.
	//
	// These rows share the action_id and differ ONLY in external_ref. The gate name and verdict are unique to
	// them, so a leak is unambiguous in either dimension, and the existing `len(rec.GateVerdicts) != 3`
	// assertion below becomes a leak detector rather than a bare count.
	for _, g := range []struct {
		ord      int
		gate, vd string
	}{{1, "admission", "refuse"}, {2, "territory-from-another-incident", "refuse"}, {3, "execute", "refuse"}} {
		if _, err := p.Exec(ctx, `INSERT INTO interceptor_gate_verdict (action_id, external_ref, ordinal, gate, verdict, reason)
			VALUES ($1,$2,$3,$4,$5,'')`, actionID, ref+"-a-different-incident", g.ord, g.gate, g.vd); err != nil {
			t.Fatalf("insert foreign-incident gate: %v", err)
		}
	}
	// credential resolution (credential_resolution) — the non-secret identity, keyed by external_ref (migration 0032).
	if _, err := p.Exec(ctx, `INSERT INTO credential_resolution
		(target, plane, outcome, source, native, rule_id, resolved_user, scheme, key_ref_scheme, shadowed, err, external_ref)
		VALUES ('librespeed01','machine','resolved','native-hostdiag',true,'rule-1','ops','ssh','env','','',$1)`, ref); err != nil {
		t.Fatalf("insert credential_resolution: %v", err)
	}

	// regime-select boundary (regime_actuation, action_id-keyed, migration 0020) — the resolved effect lane (TG-412).
	if _, err := p.Exec(ctx, `INSERT INTO regime_actuation (action_id, lane, op_class) VALUES ($1,'gitops-mr','service-restart')`, actionID); err != nil {
		t.Fatalf("insert regime_actuation: %v", err)
	}

	// Load the spine and assemble the walk — the real read path.
	rec, err := NewTraceSpineStore(p).Load(ctx, ref)
	if err != nil {
		t.Fatalf("load spine: %v", err)
	}
	if !rec.Classification.Present || !rec.Triage.Present || !rec.Prediction.Present || !rec.Verdict.Present {
		t.Fatalf("a spine boundary did not read back: %+v", rec)
	}
	// The external_ref join must have reached the prediction row (the newest field, T-020-5). If the SELECT
	// dropped external_ref the prediction would be absent — the exact field-drop this test guards.
	if rec.Prediction.ActionID != actionID {
		t.Fatalf("prediction action_id = %q, want %q — external_ref join dropped?", rec.Prediction.ActionID, actionID)
	}
	if rec.Triage.Confidence != 0.82 {
		t.Fatalf("triage confidence = %v, want 0.82 — field dropped", rec.Triage.Confidence)
	}
	if rec.Verdict.Verdict != "match" {
		t.Fatalf("verdict = %q, want match", rec.Verdict.Verdict)
	}

	// The finer enrichment rows must read back through the real SQL (the depth-projection this test guards).
	if len(rec.AgentCycles) != 2 || rec.AgentCycles[0].Cycle != 1 || rec.AgentCycles[0].Tool != "get-device-status" ||
		rec.AgentCycles[0].Observation != "nginx failed" || rec.AgentCycles[1].Cycle != 2 {
		t.Fatalf("agent_step did not read back cycle-ordered: %+v", rec.AgentCycles)
	}
	if !rec.Policy.Present || rec.Policy.Verdict != "auto" || rec.Policy.BundleVersion != "bundle-rt" ||
		len(rec.Policy.MatchedRules) != 2 || rec.Policy.MatchedRules[0] != "rt → auto" || rec.Policy.Reason != "composed auto" {
		t.Fatalf("policy_decision did not read back (matched_rules jsonb dropped?): %+v", rec.Policy)
	}
	// regime-select boundary (TG-412): the regime_actuation row must read back through the real SQL.
	if !rec.Regime.Present || rec.Regime.Lane != "gitops-mr" || rec.Regime.OpClass != "service-restart" {
		t.Fatalf("regime_actuation did not read back: %+v", rec.Regime)
	}
	for _, g := range rec.GateVerdicts {
		if g.Gate == "territory-from-another-incident" || g.Verdict == "refuse" {
			t.Fatalf("a gate row from a DIFFERENT incident of the same content-addressed action leaked into this "+
				"walk (gate=%q verdict=%q) — the read must be scoped by external_ref, not by action_id alone",
				g.Gate, g.Verdict)
		}
	}
	if len(rec.GateVerdicts) != 3 || rec.GateVerdicts[0].Gate != "admission" || rec.GateVerdicts[2].Gate != "execute" ||
		rec.GateVerdicts[0].Ordinal != 1 || rec.GateVerdicts[2].Ordinal != 3 {
		t.Fatalf("interceptor_gate_verdict did not read back ordinal-ordered for THIS incident: %+v", rec.GateVerdicts)
	}
	// TG-178: the signed margin round-trips through the SELECT — the numeric gate reads back its value, the
	// margin-less gates read nil (not 0). Killing mutation: drop `margin` from the SELECT / the scan → the
	// mode-chokepoint margin reads nil → RED.
	if rec.GateVerdicts[0].Margin != nil {
		t.Fatalf("admission (no margin written) read back margin=%v, want nil", *rec.GateVerdicts[0].Margin)
	}
	if rec.GateVerdicts[1].Gate != "mode-chokepoint" || rec.GateVerdicts[1].Margin == nil || *rec.GateVerdicts[1].Margin != -0.5 {
		t.Fatalf("mode-chokepoint margin did not round-trip through the spine SELECT: %+v", rec.GateVerdicts[1])
	}
	if len(rec.Credentials) != 1 || rec.Credentials[0].Target != "librespeed01" || rec.Credentials[0].User != "ops" ||
		rec.Credentials[0].Scheme != "ssh" || rec.Credentials[0].KeyRefScheme != "env" || rec.Credentials[0].Outcome != "resolved" {
		t.Fatalf("credential_resolution did not read back by external_ref (migration 0032): %+v", rec.Credentials)
	}

	tr := trace.Assemble(ref, rec)
	// classify + screen + 2 agent-cycles + credential + propose + predict + policy + 3 gates + regime + verify = 13.
	//
	// This count was 11 and had been RED on main since 2026-07-21 (commit fb9c48a added the `screen` stage,
	// the clean-admission projection, and nobody updated the expectation). Nothing caught it for eight days
	// because the harness CI job ran `go test ./core/db/ -run 'Axis|TransitionLog|Attribution'` — this test
	// matches none of those, so it executed in NO pipeline while every pipeline reported green. The filter is
	// widened in this same change, and TestEveryDSNGatedOracleRunsInCI now proves the exclusion cannot recur.
	// It went 12→13 when TG-412 added the `regime` step (the actuation-regime-select boundary): this fixture
	// seeds a regime_actuation row, so the assembled walk now carries a Regime step between the gates and verify.
	//
	// The failure message lists the step KINDS, not just the count: "steps=13" alone sent the first reader
	// looking for a missing row, when what had happened was a legitimately ADDED stage.
	if len(tr.Steps) != 13 || tr.Status != trace.StatusExecuted || tr.Verdict != "match" {
		kinds := make([]string, 0, len(tr.Steps))
		for _, st := range tr.Steps {
			kinds = append(kinds, string(st.Kind))
		}
		t.Fatalf("assembled walk wrong: status=%q verdict=%q steps=%d kinds=%v", tr.Status, tr.Verdict, len(tr.Steps), kinds)
	}
	if tr.Host != "librespeed01" || tr.Confidence != 0.82 {
		t.Fatalf("assembled header wrong: host=%q conf=%v", tr.Host, tr.Confidence)
	}
}

// TestTraceBatchedCycleEvidenceJoinIsPerStep guards the TG-49 consumer law: one BATCHED directive writes N
// agent_step rows and N agent_step_evidence rows SHARING a cycle ordinal, and the spine read must bind each
// step to ITS OWN evidence. Joined on (external_ref, cycle) alone — the pre-TG-49 join — a 2-call batched
// cycle read back as the 2×2 cross product: four rendered steps, two of them citing the SIBLING's payload as
// their "ground truth", on the surface (TG-272) whose premise is that a claim's citation opens the claim's
// own evidence. RED against that join (returns 5 rows here, mispaired); GREEN only when the join correlates
// by the evidence id the step's own observation embeds, over BOTH orchestrator-authored observation shapes
// (TG-199's "observed <id>" and "observed <id> (tool call FAILED)"). DSN-gated.
func TestTraceBatchedCycleEvidenceJoinIsPerStep(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the batched-cycle evidence-join test")
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

	uniq := fmt.Sprintf("batchjoin-it-%d", os.Getpid())
	ref := uniq + "-ref"
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM session_triage WHERE external_ref = $1", ref)
		// best-effort: the two step tables are append-only (REVOKE DELETE from tg_runtime) — no-op under the
		// runtime role, cleans under the owner role, ignored either way (unique per-pid refs keep reruns disjoint).
		_, _ = p.Exec(ctx, "DELETE FROM agent_step WHERE external_ref = $1", ref)
		_, _ = p.Exec(ctx, "DELETE FROM agent_step_evidence WHERE external_ref = $1", ref)
	}()

	// The session anchor: Load returns ErrNotFound for an external_ref with neither a classification nor a
	// triage row, so the fixture records the minimal triage terminal a real batched investigation would have.
	if err := NewTriageStore(p).RecordTriage(ctx, judge.TriageRow{
		ExternalRef: ref, Host: "librespeed01", AlertRule: "iface_down", Band: "AUTO",
		Outcome: "no-proposal:stop", Conclusion: "batched readings clean", Confidence: 0.9,
	}); err != nil {
		t.Fatalf("record triage anchor: %v", err)
	}

	// Cycle 1 is a 2-call batch (directive order alpha, beta); cycle 2 is a single FAILED-shape call — the
	// second observation shape the join must also bind.
	for _, c := range []struct {
		cycle             int
		tool, observation string
	}{
		{1, "probe-alpha", "observed tr-alpha"},
		{1, "probe-beta", "observed tr-beta"},
		{2, "probe-gamma", "observed tr-gamma (tool call FAILED)"},
	} {
		if _, err := p.Exec(ctx, `INSERT INTO agent_step (external_ref, cycle, thought, tool, observation, outcome)
			VALUES ($1,$2,'',$3,$4,'investigate')`, ref, c.cycle, c.tool, c.observation); err != nil {
			t.Fatalf("insert agent_step: %v", err)
		}
	}
	// The ground truth behind each step, via the REAL writer (the pgx-fake-hides-field-drop lesson).
	ev := NewAgentStepEvidenceStore(p)
	for _, e := range []trace.AgentStepEvidence{
		{ExternalRef: ref, Cycle: 1, EvidenceID: "tr-alpha", Tool: "probe-alpha", Payload: "alpha payload"},
		{ExternalRef: ref, Cycle: 1, EvidenceID: "tr-beta", Tool: "probe-beta", Payload: "beta payload"},
		{ExternalRef: ref, Cycle: 2, EvidenceID: "tr-gamma", Tool: "probe-gamma", Payload: "gamma failure text"},
	} {
		if err := ev.EmitEvidence(ctx, e); err != nil {
			t.Fatalf("emit evidence %s: %v", e.EvidenceID, err)
		}
	}

	rec, err := NewTraceSpineStore(p).Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Exactly one row PER STEP — a cross-product join returns 5 here (2×2 + 1) and fails on count alone.
	if len(rec.AgentCycles) != 3 {
		t.Fatalf("batched cycle must read back one row per step, got %d rows: %+v", len(rec.AgentCycles), rec.AgentCycles)
	}
	for i, want := range []struct {
		cycle          int
		tool, evidence string
	}{
		{1, "probe-alpha", "tr-alpha"},
		{1, "probe-beta", "tr-beta"},
		{2, "probe-gamma", "tr-gamma"},
	} {
		got := rec.AgentCycles[i]
		if got.Cycle != want.cycle || got.Tool != want.tool || got.EvidenceID != want.evidence {
			t.Fatalf("row %d: want cycle=%d tool=%s evidence=%s (each step bound to ITS OWN ground truth, in "+
				"directive order), got %+v", i, want.cycle, want.tool, want.evidence, got)
		}
	}
}

// TestTracePredictJoinsByActionID guards the fix for the "commit step shows no data yet" bug: a committed
// prediction whose external_ref column is EMPTY (the writer does not stamp it on every row) must still be
// found and rendered, by joining on the classification's content-hashed action_id. Before the fix the predict
// query keyed only on external_ref and orphaned every such prediction. DSN-gated.
func TestTracePredictJoinsByActionID(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the predict action_id-join test")
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

	uniq := fmt.Sprintf("predjoin-it-%d", os.Getpid())
	ref, actionID, planHash := uniq+"-ref", uniq+"-act", uniq+"-plan"
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM session_risk_audit WHERE external_ref = $1", ref)
		_, _ = p.Exec(ctx, "DELETE FROM infragraph_prediction WHERE action_id = $1", actionID)
	}()

	// classify carries the action_id.
	if err := NewRiskAuditStore(p).PersistRiskAudit(audit.RiskAudit{
		ExternalRef: ref, RiskLevel: "low", Band: safety.BandAuto, AutoApproved: true,
		PlanHash: planHash, ActionID: actionID, SchemaVersion: 1,
	}); err != nil {
		t.Fatalf("persist risk audit: %v", err)
	}
	// a committed prediction with an EMPTY external_ref (the exact orphaned-row shape seen in prod), keyed by action_id.
	if _, err := p.Exec(ctx, `INSERT INTO infragraph_prediction
		(external_ref, action_id, plan_hash, kind, target_host, prediction_hash, committed_at, schema_version)
		VALUES ('', $1, $2, 'action', 'h1', $1, now(), 1)`, actionID, planHash); err != nil {
		t.Fatalf("insert empty-external_ref prediction: %v", err)
	}

	rec, err := NewTraceSpineStore(p).Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !rec.Prediction.Present {
		t.Fatalf("prediction with empty external_ref was NOT found by action_id join — the commit step would show 'no data yet'")
	}
	if rec.Prediction.ActionID != actionID {
		t.Errorf("prediction action_id = %q, want %q", rec.Prediction.ActionID, actionID)
	}
}

// TestTraceScreenStepFromSignals guards the screen step (MR5): the classifier's committed signals_json
// (session_risk_audit, migration 0003) must read back into rec.Classification.Signals and the assembler must
// emit a StepScreen carrying them, deterministically sorted. Before the fix the signals were written but never
// read, so the console's screen slot always showed the "no data yet" scaffold. DSN-gated.
func TestTraceScreenStepFromSignals(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the screen-step signals test")
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

	uniq := fmt.Sprintf("screen-it-%d", os.Getpid())
	ref, actionID := uniq+"-ref", uniq+"-act"
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM session_risk_audit WHERE external_ref = $1", ref) }()

	// classify with the admission-screen signals the classifier records for a POLL_PAUSE (the exact shape seen
	// on-box: {"poll_reason":"ood-novel-incident"}), plus a second key to prove the deterministic sort.
	if err := NewRiskAuditStore(p).PersistRiskAudit(audit.RiskAudit{
		ExternalRef: ref, RiskLevel: "high", Band: safety.BandPollPause, ActionID: actionID, SchemaVersion: 1,
		Signals: map[string]string{"poll_reason": "ood-novel-incident", "blast_radius": "3 hosts"},
	}); err != nil {
		t.Fatalf("persist risk audit: %v", err)
	}

	rec, err := NewTraceSpineStore(p).Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.Classification.Signals["poll_reason"] != "ood-novel-incident" {
		t.Fatalf("signals_json did not read back: %+v", rec.Classification.Signals)
	}

	tr := trace.Assemble(ref, rec)
	var screen *trace.Step
	for i := range tr.Steps {
		if tr.Steps[i].Kind == trace.StepScreen {
			screen = &tr.Steps[i]
		}
	}
	if screen == nil {
		t.Fatalf("no screen step assembled from committed signals: %+v", tr.Steps)
	}
	// deterministic sort → blast_radius before poll_reason, each rendered "key: value".
	if screen.Reason != "blast_radius: 3 hosts · poll_reason: ood-novel-incident" {
		t.Errorf("screen reason = %q, want the sorted signal projection", screen.Reason)
	}
}

// TestTraceRagStepFromSkillLoads guards the rag step (MR6): the proposal's composed-seed provenance
// (session_triage.skill_loads, migration 0010) must read back into rec.Triage.SkillLoads and the assembler
// must emit a StepRag carrying them. Before the fix skill_loads was written but never read, so the console's
// rag slot always showed the "no data yet" scaffold. DSN-gated.
func TestTraceRagStepFromSkillLoads(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the rag-step skill_loads test")
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

	uniq := fmt.Sprintf("rag-it-%d", os.Getpid())
	ref := uniq + "-ref"
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM session_triage WHERE external_ref = $1", ref) }()

	loads := []string{"restart-service@3#s1:store", "iface-flap@1#p2:precedent"}
	if err := NewTriageStore(p).RecordTriage(ctx, judge.TriageRow{
		ExternalRef: ref, Host: "librespeed01", AlertRule: "iface_down", Band: "AUTO",
		Outcome: "proposal", Proposed: true, Op: "restart-service", Conclusion: "flap cleared",
		Confidence: 0.8, SkillLoads: loads,
	}); err != nil {
		t.Fatalf("record triage: %v", err)
	}

	rec, err := NewTraceSpineStore(p).Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rec.Triage.SkillLoads) != 2 || rec.Triage.SkillLoads[0] != "restart-service@3#s1:store" {
		t.Fatalf("skill_loads did not read back: %+v", rec.Triage.SkillLoads)
	}

	tr := trace.Assemble(ref, rec)
	var rag *trace.Step
	for i := range tr.Steps {
		if tr.Steps[i].Kind == trace.StepRag {
			rag = &tr.Steps[i]
		}
	}
	if rag == nil || len(rag.Skills) != 2 {
		t.Fatalf("no rag step assembled from committed skill_loads: %+v", tr.Steps)
	}
	// and the provenance must NOT double-home on the propose step (re-homed to rag).
	for _, s := range tr.Steps {
		if s.Kind == trace.StepPropose && len(s.Skills) != 0 {
			t.Errorf("propose step must not carry skills (re-homed to rag): %+v", s.Skills)
		}
	}
}

// TestTraceCommitPlanOpsAndVerifyTs guards MR1+MR2: the commit step must carry the sealed action's structured
// plan ops (projected from action_manifest.action) and the verify step must carry action_verdict.created_at.
// DSN-gated.
func TestTraceCommitPlanOpsAndVerifyTs(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the commit-plan-ops / verify-ts test")
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

	// the manifest's action_id is the CONTENT HASH of the action — use it as THE session action_id so classify,
	// verify and the manifest all correlate.
	mf, err := manifest.New(
		manifest.Action{Target: "librespeed01", OpClass: "restart-service", Op: "restart",
			Params: map[string]string{"unit": "nginx"}, Reversible: true},
		safety.BandAuto, "ph-planops-it", "predh-planops-it")
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	actionID := mf.ActionID
	ref := fmt.Sprintf("planops-it-%d-ref", os.Getpid())
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM session_risk_audit WHERE external_ref = $1", ref)
		_, _ = p.Exec(ctx, "DELETE FROM action_verdict WHERE action_id = $1", actionID)
		_, _ = p.Exec(ctx, "DELETE FROM action_manifest WHERE action_id = $1", actionID)
	}()

	if err := NewManifestStore(p).Seal(ctx, mf); err != nil {
		t.Fatalf("seal manifest: %v", err)
	}
	if err := NewRiskAuditStore(p).PersistRiskAudit(audit.RiskAudit{
		ExternalRef: ref, RiskLevel: "low", Band: safety.BandAuto, AutoApproved: true,
		PlanHash: "ph-planops-it", ActionID: actionID, SchemaVersion: 1,
	}); err != nil {
		t.Fatalf("persist classify: %v", err)
	}
	if err := NewVerdictStore(p).Commit(ctx, actionID, "ph-planops-it", "librespeed01", "nl", safety.Verdict("match")); err != nil {
		t.Fatalf("commit verdict: %v", err)
	}

	rec, err := NewTraceSpineStore(p).Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// commit plan ops projected from the sealed manifest
	if !rec.Commit.Present || len(rec.Commit.PlanOps) != 1 {
		t.Fatalf("commit plan ops not projected from action_manifest: present=%v ops=%+v", rec.Commit.Present, rec.Commit.PlanOps)
	}
	if rec.Commit.PlanOps[0].Op != "change" {
		t.Errorf("plan op polarity = %q, want change (restart-service)", rec.Commit.PlanOps[0].Op)
	}
	// verify timestamp threaded from action_verdict.created_at
	if !rec.Verdict.Present || rec.Verdict.CreatedAt.IsZero() {
		t.Fatalf("verify created_at not threaded: present=%v createdAt=%v", rec.Verdict.Present, rec.Verdict.CreatedAt)
	}
}
