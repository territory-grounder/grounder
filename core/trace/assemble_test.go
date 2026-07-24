package trace

import (
	"strings"
	"testing"
	"time"
)

func ts(sec int) time.Time { return time.Unix(int64(sec), 0).UTC() }

// A fully-executed session assembles all four boundaries in decision order, and the header is sourced from
// the durable rows (band/action from classification, host/confidence from triage, verdict from the verifier).
func TestAssembleFullWalk(t *testing.T) {
	rec := SpineRecords{
		Classification: ClassificationRecord{
			Present: true, Band: "AUTO", RiskLevel: "low", ActionID: "act-1", PlanHash: "ph-1",
			AutoApproved: true, CreatedAt: ts(100),
		},
		Triage: TriageRecord{
			Present: true, Host: "librespeed01", AlertRule: "iface_down", Band: "AUTO",
			Proposed: true, Op: "restart-service", Conclusion: "flap cleared by restart",
			Confidence: 0.82, SkillLoads: []string{"restart@3#s1:store"}, CreatedAt: ts(110),
		},
		Prediction: PredictionRecord{Present: true, ActionID: "act-1", PlanHash: "ph-1", Scored: true, TP: 1, CommittedAt: ts(120)},
		Verdict:    VerdictRecord{Present: true, Verdict: "match"},
	}
	tr := Assemble("ext-1", rec)

	if tr.ExternalRef != "ext-1" || tr.Band != "AUTO" || tr.ActionID != "act-1" || tr.PlanHash != "ph-1" {
		t.Fatalf("header from classification wrong: %+v", tr)
	}
	if tr.Host != "librespeed01" || tr.AlertRule != "iface_down" || tr.Confidence != 0.82 {
		t.Fatalf("header from triage wrong: %+v", tr)
	}
	if tr.Verdict != "match" || tr.Status != StatusExecuted {
		t.Fatalf("verdict/status wrong: verdict=%q status=%q", tr.Verdict, tr.Status)
	}
	// The composed-seed provenance (SkillLoads) surfaces as a dedicated rag (retrieval) boundary between classify
	// and propose — not on the propose step.
	// classify → screen (admission) → rag → propose → predict → verify.
	if len(tr.Steps) != 6 {
		t.Fatalf("want 6 steps, got %d: %+v", len(tr.Steps), tr.Steps)
	}
	wantKinds := []StepKind{StepClassify, StepScreen, StepRag, StepPropose, StepPredict, StepVerify}
	for i, s := range tr.Steps {
		if s.Seq != i {
			t.Fatalf("step %d has Seq %d (order must be ascending 0-based)", i, s.Seq)
		}
		if s.Kind != wantKinds[i] {
			t.Fatalf("step %d kind = %q, want %q", i, s.Kind, wantKinds[i])
		}
	}
	// The rag step (index 2) carries the retrieval provenance; the propose step (index 3) no longer does.
	if len(tr.Steps[2].Skills) != 1 || tr.Steps[2].Skills[0] != "restart@3#s1:store" {
		t.Fatalf("rag step lost skill-load provenance: %+v", tr.Steps[2])
	}
	if len(tr.Steps[3].Skills) != 0 {
		t.Fatalf("propose step must not carry skills (re-homed to rag): %+v", tr.Steps[3].Skills)
	}
	// The propose step carries the stated confidence; the predict step scored clean (tp=1,fp=0,fn=0).
	if tr.Steps[3].Confidence != 0.82 {
		t.Fatalf("propose confidence = %v, want 0.82", tr.Steps[3].Confidence)
	}
	if tr.Steps[4].Verdict != "clean" {
		t.Fatalf("predict verdict = %q, want clean", tr.Steps[4].Verdict)
	}
}

// A queued session (classified, no proposal) yields only its prefix — never a fabricated proposal/verdict
// tail (INV-15). Status is derived off the durable rows alone.
func TestAssembleQueuedPrefixOnly(t *testing.T) {
	rec := SpineRecords{Classification: ClassificationRecord{Present: true, Band: "POLL_PAUSE", RiskLevel: "high", ActionID: "act-2", CreatedAt: ts(200)}}
	tr := Assemble("ext-2", rec)
	if tr.Status != StatusClassified {
		t.Fatalf("status = %q, want classified", tr.Status)
	}
	// a classified session yields classify + its admission screen (a clean-admission projection when no signals).
	if len(tr.Steps) != 2 || tr.Steps[0].Kind != StepClassify || tr.Steps[1].Kind != StepScreen {
		t.Fatalf("queued session must yield classify + screen, got %+v", tr.Steps)
	}
	if tr.Verdict != "" {
		t.Fatalf("queued session must carry no verdict, got %q", tr.Verdict)
	}
}

// A STOP (proposal row present but Proposed=false) is its own lifecycle state, not a running proposal.
func TestAssembleStopStatus(t *testing.T) {
	rec := SpineRecords{
		Classification: ClassificationRecord{Present: true, Band: "NOTIFY", CreatedAt: ts(300)},
		Triage:         TriageRecord{Present: true, Proposed: false, Conclusion: "no safe action", CreatedAt: ts(310)},
	}
	tr := Assemble("ext-3", rec)
	if tr.Status != StatusStopped {
		t.Fatalf("status = %q, want stopped", tr.Status)
	}
	// classify → screen → propose(Stop).
	if len(tr.Steps) != 3 || tr.Steps[1].Kind != StepScreen || tr.Steps[2].Label != "Stop (no action proposed)" {
		t.Fatalf("stop walk wrong: %+v", tr.Steps)
	}
}

// A running session (proposed, no verdict) reports proposed status and omits the verify step.
func TestAssembleRunningNoVerdict(t *testing.T) {
	rec := SpineRecords{
		Classification: ClassificationRecord{Present: true, Band: "AUTO", CreatedAt: ts(400)},
		Triage:         TriageRecord{Present: true, Proposed: true, Op: "restart-service", Confidence: 0.6, CreatedAt: ts(410)},
		Prediction:     PredictionRecord{Present: true, Scored: false, CommittedAt: ts(420)},
	}
	tr := Assemble("ext-4", rec)
	if tr.Status != StatusProposed {
		t.Fatalf("status = %q, want proposed", tr.Status)
	}
	for _, s := range tr.Steps {
		if s.Kind == StepVerify {
			t.Fatalf("running session must not carry a verify step: %+v", tr.Steps)
		}
	}
	// An unscored prediction reports awaiting-window, not a fabricated verdict.
	if tr.Steps[2].Verdict != "" {
		t.Fatalf("unscored prediction must carry no verdict, got %q", tr.Steps[2].Verdict)
	}
}

// Assemble is deterministic: the same input yields byte-identical output every call (the observe-only,
// no-side-effect property the REQ-2002 read path depends on).
func TestAssembleDeterministic(t *testing.T) {
	rec := SpineRecords{
		Classification: ClassificationRecord{Present: true, Band: "AUTO", ActionID: "a", CreatedAt: ts(500)},
		Triage:         TriageRecord{Present: true, Proposed: true, Confidence: 0.5, CreatedAt: ts(510)},
	}
	a := Assemble("ext-5", rec)
	b := Assemble("ext-5", rec)
	if len(a.Steps) != len(b.Steps) || a.Status != b.Status || a.Confidence != b.Confidence {
		t.Fatalf("assemble not deterministic: %+v vs %+v", a, b)
	}
}

// REQ-2000/REQ-2009 completion: the propose step carries the composed-seed prompt/seed/model provenance, and
// the policy step carries the min_confidence threshold in force — previously declared but never populated.
func TestAssembleReq2000Provenance(t *testing.T) {
	rec := SpineRecords{
		Classification: ClassificationRecord{Present: true, Band: "AUTO", ActionID: "act-1", CreatedAt: ts(1)},
		Triage: TriageRecord{
			Present: true, Proposed: true, Op: "restart", Conclusion: "restart it", Confidence: 0.8, CreatedAt: ts(2),
			PromptVersion: "triage@4", SeedHash: "abc123", ModelTier: "kimi-k3",
		},
		Policy: PolicyRecord{Present: true, Verdict: "auto", MinConfidence: 0.6, CreatedAt: ts(3)},
	}
	tr := Assemble("ext-req2000", rec)
	var propose, policy *Step
	for i := range tr.Steps {
		switch tr.Steps[i].Kind {
		case StepPropose:
			propose = &tr.Steps[i]
		case StepPolicy:
			policy = &tr.Steps[i]
		}
	}
	if propose == nil || len(propose.Prompts) != 3 || propose.Prompts[0] != "prompt: triage@4" ||
		propose.Prompts[1] != "seed: abc123" || propose.Prompts[2] != "model: kimi-k3" {
		t.Fatalf("propose step missing prompt/seed/model provenance: %+v", propose)
	}
	if policy == nil || policy.MinConfidence != 0.6 {
		t.Fatalf("policy step missing min_confidence: %+v", policy)
	}
	// only the recorded fields surface (no fabricated provenance)
	if got := proposeProvenance("", "", ""); got != nil {
		t.Errorf("empty identity must yield no provenance, got %v", got)
	}
}

// The correlate boundary (StepCorrelate, governance_ledger "suppress:"+ref) is emitted between ingest and
// classify ONLY when a suppression/correlation decision was durably recorded; an escalate outcome (the tracer
// case — the alert proceeded, not suppressed) carries a "clean" verdict and its phase:reason. Absent → no step.
func TestAssembleCorrelateStep(t *testing.T) {
	rec := SpineRecords{
		Ingest:         IngestRecord{Present: true, AlertRule: "HighMem", Host: "librespeed01", ReceivedAt: ts(100)},
		Correlate:      CorrelateRecord{Present: true, Outcome: "escalate", Reason: "dedup:no dedup match", DecidedAt: ts(101)},
		Classification: ClassificationRecord{Present: true, Band: "POLL_PAUSE", ActionID: "a", CreatedAt: ts(102)},
	}
	tr := Assemble("ext-corr", rec)
	// order: ingest → correlate → classify
	if len(tr.Steps) < 3 || tr.Steps[0].Kind != StepIngest || tr.Steps[1].Kind != StepCorrelate || tr.Steps[2].Kind != StepClassify {
		t.Fatalf("want ingest→correlate→classify, got %+v", tr.Steps)
	}
	if co := tr.Steps[1]; co.Reason != "dedup:no dedup match" || co.Verdict != "clean" {
		t.Fatalf("correlate step lost its decision: %+v", co)
	}
	// no correlate row → no correlate step (never fabricated).
	bare := SpineRecords{Classification: ClassificationRecord{Present: true, ActionID: "a", CreatedAt: ts(102)}}
	for _, s := range Assemble("ext-b", bare).Steps {
		if s.Kind == StepCorrelate {
			t.Fatalf("fabricated a correlate step with no durable decision: %+v", s)
		}
	}
}

// The admission screen (StepScreen, migration 0003 signals_json) is emitted right after classify, ONLY when
// the classifier committed screen signals, and its reason is the signals projected deterministically (sorted
// by key, "key: value"). Absent signals → a clean-admission projection (never a fabricated signal).
func TestAssembleScreenStep(t *testing.T) {
	// with signals → a screen step directly after classify, sorted deterministically.
	rec := SpineRecords{Classification: ClassificationRecord{
		Present: true, Band: "POLL_PAUSE", RiskLevel: "high", ActionID: "act-s", CreatedAt: ts(300),
		Signals: map[string]string{"poll_reason": "ood-novel-incident", "blast_radius": "3 hosts"},
	}}
	tr := Assemble("ext-screen", rec)
	if len(tr.Steps) != 2 || tr.Steps[0].Kind != StepClassify || tr.Steps[1].Kind != StepScreen {
		t.Fatalf("want classify→screen, got %+v", tr.Steps)
	}
	if got := tr.Steps[1].Reason; got != "blast_radius: 3 hosts · poll_reason: ood-novel-incident" {
		t.Fatalf("screen reason = %q, want the sorted signal projection", got)
	}
	if tr.Steps[1].Seq != 1 {
		t.Fatalf("screen seq = %d, want 1 (right after classify)", tr.Steps[1].Seq)
	}

	// no signals (a clean AUTO admission) → a screen step projecting the admission decision, never "no data yet".
	bare := SpineRecords{Classification: ClassificationRecord{Present: true, Band: "AUTO", RiskLevel: "low", AutoApproved: true, ActionID: "a", CreatedAt: ts(300)}}
	tb := Assemble("ext-bare", bare)
	if len(tb.Steps) != 2 || tb.Steps[1].Kind != StepScreen {
		t.Fatalf("clean-admission session must yield classify + screen, got %+v", tb.Steps)
	}
	if got := tb.Steps[1].Reason; got != "clean admission — no pause/floor/novelty signal · auto-approved · risk: low" {
		t.Fatalf("clean-admission screen reason = %q", got)
	}
}

// The commit boundary is emitted from a REAL committed artifact — a sealed manifest (plan-ops) OR a committed
// prediction — never fabricated from the classification alone. (B1/B3): a manifest-only session surfaces its
// sealed plan-ops on a "Sealed action" step even with no prediction; a classify-only session emits NO commit
// step (no fabricated "predicted end-state committed").
func TestAssembleCommitBoundaryHonest(t *testing.T) {
	// manifest sealed, no consequence prediction → a Sealed-action step carrying the plan ops (B3: not dropped).
	manifestOnly := SpineRecords{
		Classification: ClassificationRecord{Present: true, Band: "AUTO", ActionID: "act-m", CreatedAt: ts(400)},
		Commit:         CommitRecord{Present: true, PlanOps: []PlanOp{{Op: "change", T: "unit: nginx"}}},
	}
	tr := Assemble("ext-m", manifestOnly)
	var commit *Step
	for i := range tr.Steps {
		if tr.Steps[i].Kind == StepPredict {
			commit = &tr.Steps[i]
		}
	}
	if commit == nil {
		t.Fatalf("manifest-only session emitted no commit boundary — sealed plan-ops dropped (B3): %+v", tr.Steps)
	}
	if commit.Label != "Sealed action" || len(commit.PlanOps) != 1 || commit.PlanOps[0].Op != "change" {
		t.Fatalf("commit boundary lost the sealed plan-ops: %+v", commit)
	}
	if commit.Verdict != "" {
		t.Fatalf("manifest-only commit must NOT fabricate a prediction verdict: %q", commit.Verdict)
	}

	// classified only — no manifest, no prediction → NO commit boundary at all (B1: no fabrication).
	classifyOnly := SpineRecords{Classification: ClassificationRecord{Present: true, Band: "POLL_PAUSE", ActionID: "act-c", CreatedAt: ts(400)}}
	tc := Assemble("ext-c", classifyOnly)
	for _, s := range tc.Steps {
		if s.Kind == StepPredict {
			t.Fatalf("classify-only session fabricated a commit/predict boundary (INV-15): %+v", s)
		}
	}
}

// A fully-executed session with the finer T-020-7/8/3 rows enriches the walk additively: classify → agent
// ReAct cycles → propose → predict → policy authorization → interceptor gates → verify, in that order, each
// carrying its decision-grade fields. The coarse-only cases above prove the absent-enrichment prefix is
// unchanged (the two-layer additive design, REQ-2001/REQ-2017).
func TestAssembleEnrichedWalk(t *testing.T) {
	rec := SpineRecords{
		Classification: ClassificationRecord{Present: true, Band: "AUTO", RiskLevel: "low", ActionID: "act-1", CreatedAt: ts(1)},
		AgentCycles: []AgentCycleRecord{
			{Cycle: 1, Thought: "check nginx", Tool: "get-device-status", Observation: "nginx failed", Outcome: "success", CreatedAt: ts(2)},
			{Cycle: 2, Thought: "conclude restart", Outcome: "success", CreatedAt: ts(3)},
		},
		Credentials: []CredentialRecord{
			{Target: "web01", Outcome: "resolved", User: "ops", Scheme: "ssh", KeyRefScheme: "env", Source: "native-hostdiag", CreatedAt: ts(3)},
		},
		Triage:     TriageRecord{Present: true, Host: "web01", AlertRule: "svc-down", Band: "AUTO", Outcome: "proposal", Proposed: true, Op: "restart-service", Conclusion: "restart it", Confidence: 0.8, CreatedAt: ts(4)},
		Prediction: PredictionRecord{Present: true, ActionID: "act-1", Scored: true, TP: 1, CommittedAt: ts(5)},
		Policy:     PolicyRecord{Present: true, Verdict: "auto", BundleVersion: "bundle-abc", MatchedRules: []string{"rt → auto", "floor → deny"}, Reason: "composed auto", Mode: "Semi-auto", CreatedAt: ts(6)},
		GateVerdicts: []GateVerdictRecord{
			{Ordinal: 1, Gate: "admission", Verdict: "pass", CreatedAt: ts(7)},
			{Ordinal: 2, Gate: "mode-chokepoint", Verdict: "pass", CreatedAt: ts(8)},
			{Ordinal: 3, Gate: "execute", Verdict: "pass", CreatedAt: ts(9)},
		},
		Verdict: VerdictRecord{Present: true, Verdict: "match"},
	}
	tr := Assemble("ext-enriched", rec)

	wantKinds := []StepKind{
		StepClassify, StepScreen, StepAgentCycle, StepAgentCycle, StepCredential, StepPropose,
		StepPredict, StepPolicy, StepGate, StepGate, StepGate, StepVerify,
	}
	if len(tr.Steps) != len(wantKinds) {
		got := make([]StepKind, len(tr.Steps))
		for i, s := range tr.Steps {
			got[i] = s.Kind
		}
		t.Fatalf("enriched walk: got %d steps %v, want %d %v", len(tr.Steps), got, len(wantKinds), wantKinds)
	}
	for i, s := range tr.Steps {
		if s.Kind != wantKinds[i] {
			t.Fatalf("step %d kind = %q, want %q", i, s.Kind, wantKinds[i])
		}
		if s.Seq != i {
			t.Fatalf("step %d seq = %d, want %d (ordered, ascending)", i, s.Seq, i)
		}
	}

	// The credential step carries the non-secret identity (user + connection scheme + key-ref SCHEME only) — no
	// key material or full ref value ever reaches the walk (INV-13).
	if cr := tr.Steps[4]; cr.Kind != StepCredential || cr.CredentialScheme != "ssh" || cr.CredentialRef != "env" ||
		!strings.Contains(cr.Reason, "ops") {
		t.Fatalf("credential step lost non-secret identity: %+v", cr)
	}
	// The policy step carries the rich deny-overrides provenance (bundle + full matched-rule list + verdict) and
	// surfaces the in-force mode in its reason.
	if pol := tr.Steps[7]; pol.BundleVersion != "bundle-abc" || len(pol.MatchedRules) != 2 || pol.Verdict != "auto" ||
		!strings.Contains(pol.Reason, "[mode: Semi-auto]") {
		t.Fatalf("policy step lost provenance/mode: %+v", pol)
	}
	// The first agent-cycle step (now index 2, after classify+screen) carries the invoked tool and a scrubbed reason.
	if cy := tr.Steps[2]; len(cy.Tools) != 1 || cy.Tools[0] != "get-device-status" || cy.Reason == "" {
		t.Fatalf("agent-cycle step lost tool/reason: %+v", cy)
	}
	// Each gate step carries its gate name and verdict, ordinal-ordered.
	if g := tr.Steps[8]; g.Kind != StepGate || g.Gate != "admission" || g.Verdict != "pass" {
		t.Fatalf("first gate step wrong: %+v", g)
	}
	if g := tr.Steps[10]; g.Gate != "execute" {
		t.Fatalf("gate order wrong at step 10: %+v", g)
	}
}
