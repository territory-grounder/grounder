package skilljudge

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// memTriage is the in-memory Store fake (CI has no Postgres).
type memTriage struct {
	mu        sync.Mutex
	rows      []judge.TriageRow
	judgments map[string]map[string]float64 // ref → dimension → score
	judged    map[string]bool
	failWrite string // ref whose judgment writes fail (fault injection)
}

func newMemTriage(rows ...judge.TriageRow) *memTriage {
	return &memTriage{rows: rows, judgments: map[string]map[string]float64{}, judged: map[string]bool{}}
}

func (m *memTriage) UnjudgedSince(_ context.Context, _ time.Duration, limit int) ([]judge.TriageRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []judge.TriageRow
	for _, r := range m.rows {
		if !m.judged[r.ExternalRef] && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memTriage) WriteJudgment(_ context.Context, ref, dim string, score float64, _, rubricVersion string) error {
	if rubricVersion == "" {
		return fmt.Errorf("empty rubric version reached the store — the stamp is required (TG-194)")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ref == m.failWrite {
		return fmt.Errorf("injected write failure")
	}
	if m.judgments[ref] == nil {
		m.judgments[ref] = map[string]float64{}
	}
	m.judgments[ref][dim] = score
	return nil
}

func (m *memTriage) MarkJudged(_ context.Context, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.judged[ref] = true
	return nil
}

// scriptedJudge returns a canned verdict per session ref (matched from the prompt), or an error.
type scriptedJudge struct {
	verdicts map[string]string // substring of the prompt → raw reply
	err      map[string]error
	calls    int
}

func (s *scriptedJudge) Complete(_ context.Context, _, _ string, msgs []model.Message) (string, error) {
	s.calls++
	prompt := msgs[len(msgs)-1].Content
	for key, e := range s.err {
		if strings.Contains(prompt, key) {
			return "", e
		}
	}
	for key, v := range s.verdicts {
		if strings.Contains(prompt, key) {
			return v, nil
		}
	}
	return "no verdict scripted", nil
}

const goodVerdict = `{"correct_diagnosis":4,"evidence_grounded":4,"sensible_proposal":4,"appropriate_band":5,"falsifiable_prediction":3,"comment":"solid"}`
const lowVerdict = `{"correct_diagnosis":1,"evidence_grounded":1,"sensible_proposal":1,"appropriate_band":1,"falsifiable_prediction":1,"comment":"poor"}`

func row(ref string, loads ...string) judge.TriageRow {
	// The ref rides in the alert rule so the scripted judge can key its verdict off the prompt (the
	// judge prompt carries the incident facts, not the raw ref).
	return judge.TriageRow{ExternalRef: ref, Host: "web01", AlertRule: "HostDown/" + ref, Band: "AUTO_NOTICE",
		Outcome: "proposed", Proposed: true, Op: "restart-service", SkillLoads: loads}
}

// The batch judges every unjudged session: one judgment row per dimension, the session marked, the
// summary honest. A model failure or an unparseable verdict skips THAT session (retried next run) and
// never aborts the batch.
func TestJudgeBatchScoresAndSkips(t *testing.T) {
	st := newMemTriage(
		row("TG-good"),
		row("TG-modelfail"),
		row("TG-garbled"),
	)
	mdl := &scriptedJudge{
		verdicts: map[string]string{"TG-good": goodVerdict, "TG-garbled": "I refuse to answer in JSON"},
		err:      map[string]error{"TG-modelfail": fmt.Errorf("429 overloaded")},
	}
	acts := &Activities{D: Deps{Model: mdl, Store: st}}
	out, err := acts.JudgeBatchActivity(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if out.Judged != 1 || out.Skipped != 2 {
		t.Fatalf("want 1 judged / 2 skipped, got %+v", out)
	}
	if got := st.judgments["TG-good"]; len(got) != 5 || got["appropriate_band"] != 5 {
		t.Fatalf("all five dimensions must be written: %v", got)
	}
	if !st.judged["TG-good"] || st.judged["TG-modelfail"] || st.judged["TG-garbled"] {
		t.Fatalf("only the judged session is marked: %v", st.judged)
	}
	// The skipped sessions surface in the next batch (retried, not lost).
	next, _ := st.UnjudgedSince(context.Background(), JudgeWindow, BatchLimit)
	if len(next) != 2 {
		t.Fatalf("skipped sessions must remain unjudged: %v", next)
	}
}

// A judgment-write failure leaves the session unmarked (re-judged next run) and continues the batch.
func TestJudgeBatchWriteFailureSkips(t *testing.T) {
	st := newMemTriage(row("TG-a"), row("TG-b"))
	st.failWrite = "TG-a"
	mdl := &scriptedJudge{verdicts: map[string]string{"TG-a": goodVerdict, "TG-b": goodVerdict}}
	acts := &Activities{D: Deps{Model: mdl, Store: st}}
	out, err := acts.JudgeBatchActivity(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if out.Judged != 1 || out.Skipped != 1 || st.judged["TG-a"] || !st.judged["TG-b"] {
		t.Fatalf("the failed write skips its session only: %+v judged=%v", out, st.judged)
	}
}

// graduatedWatch builds a graduated store (v1 retired, v2 production) with an armed watch on v2.
func graduatedWatch(t *testing.T, dimension string) (*skillstore.MemStore, *audit.Ledger, *skillstore.MemWatchStore, skillstore.Version) {
	t.Helper()
	m := skillstore.NewMemStore()
	m.PutSkill(skillstore.Skill{Name: "triage-protocol", Kind: "behavioral", Position: 5})
	lg := audit.NewLedger()
	ctx := context.Background()
	mk := func(ver, body string) skillstore.Version {
		aw := skillstore.AppliesWhen{}
		v, err := m.CreateVersion(ctx, skillstore.Version{SkillName: "triage-protocol", Version: ver,
			Body: body, AppliesWhen: aw, ContentHash: skillstore.ContentHash(body, aw),
			Author: "t", Source: "t", Rationale: "test"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := skillstore.Transition(ctx, m, lg, v.ID, skillstore.StatusTrial, "gate"); err != nil {
			t.Fatal(err)
		}
		v, err = skillstore.Transition(ctx, m, lg, v.ID, skillstore.StatusProduction, "grad")
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	v1 := mk("1.0.0", "body v1")
	v2 := mk("2.0.0", "body v2")
	ws := skillstore.NewMemWatchStore()
	if err := skillstore.OpenWatch(ctx, ws, v2.ID, v1.ID, "triage-protocol", dimension, 3.5, 0.05, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return m, lg, ws, v2
}

// A judged session that composed a watched store version feeds the regression watch on the watch's
// OWN dimension: regressing scores accrue failures; enough consecutive ones demote the graduate.
func TestJudgeBatchFeedsWatch(t *testing.T) {
	m, lg, ws, v2 := graduatedWatch(t, "correct_diagnosis")
	load := fmt.Sprintf("triage-protocol@2.0.0#%d:store", v2.ID)

	var rows []judge.TriageRow
	for i := 0; i < skillstore.DefaultWatchThreshold; i++ {
		rows = append(rows, row(fmt.Sprintf("TG-w%d", i), load))
	}
	// One session that did NOT compose the watched version — it must not count.
	rows = append(rows, row("TG-unrelated", "triage-protocol@1.0.0:compiled"))
	st := newMemTriage(rows...)
	mdl := &scriptedJudge{verdicts: map[string]string{"TG-": lowVerdict}}
	var escalated string
	acts := &Activities{D: Deps{Model: mdl, Store: st, Watch: ws, Skills: m, Ledger: lg,
		Escalate: func(_ context.Context, ref, reason string) error { escalated = ref + ": " + reason; return nil }}}

	out, err := acts.JudgeBatchActivity(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if out.Judged != skillstore.DefaultWatchThreshold+1 {
		t.Fatalf("all sessions must judge, got %+v", out)
	}
	if out.WatchFed != skillstore.DefaultWatchThreshold {
		t.Fatalf("only sessions composing a store version feed the watch, got %+v", out)
	}
	got, err := m.GetVersion(context.Background(), v2.ID)
	if err != nil || got.Status != skillstore.StatusRetired {
		t.Fatalf("the regressing graduate must be demoted, got %v %v", got.Status, err)
	}
	prod, ok, _ := m.ProductionVersion(context.Background(), "triage-protocol")
	if !ok || prod.Body != "body v1" {
		t.Fatalf("the prior body must return to production, got %+v", prod)
	}
	if !strings.Contains(escalated, "regression watch tripped") {
		t.Fatalf("the demotion must escalate, got %q", escalated)
	}
	if err := lg.Verify(); err != nil {
		t.Fatalf("ledger chain must verify: %v", err)
	}
}

// A watch on a dimension the trial measured is fed ONLY that dimension: sessions judged low on other
// axes but fine on the watch's dimension never accrue failures.
func TestJudgeBatchWatchDimensionScoped(t *testing.T) {
	m, _, ws, v2 := graduatedWatch(t, "appropriate_band")
	load := fmt.Sprintf("triage-protocol@2.0.0#%d:store", v2.ID)
	var rows []judge.TriageRow
	for i := 0; i < 2*skillstore.DefaultWatchThreshold; i++ {
		rows = append(rows, row(fmt.Sprintf("TG-d%d", i), load))
	}
	st := newMemTriage(rows...)
	// appropriate_band=5 (fine on the watched dimension) while everything else is 1.
	mdl := &scriptedJudge{verdicts: map[string]string{"TG-": `{"correct_diagnosis":1,"evidence_grounded":1,"sensible_proposal":1,"appropriate_band":5,"falsifiable_prediction":1}`}}
	lg := audit.NewLedger()
	acts := &Activities{D: Deps{Model: mdl, Store: st, Watch: ws, Skills: m, Ledger: lg}}
	if _, err := acts.JudgeBatchActivity(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetVersion(context.Background(), v2.ID)
	if err != nil || got.Status != skillstore.StatusProduction {
		t.Fatalf("a graduate fine on its watched dimension must stay production, got %v %v", got.Status, err)
	}
}

// TG-201 — THE DIMENSION IS ACTUALLY EVALUATED, NOT MERELY DEFINED.
//
// This is the WIRING oracle: a scorer nobody calls is the same non-event as an unscored diagnosis. The
// batch must compute diagnosis_grounded off the durable record and write a session_judgment row for it,
// beside the five the model scores.
//
// KILLING MUTATION: delete the ScoreDiagnosis block from JudgeBatchActivity (or make it always write 5).
// RED — a session whose stated root cause its own captured evidence refutes is judged, marked, and pooled
// exactly like a clean one, so the typed claim costs the agent nothing and the A2 failure ships free.
func TestJudgeBatchWritesTheDeterministicDiagnosisDimension(t *testing.T) {
	contradicted := row("TG-contradicted")
	contradicted.DiagnosisRecorded = true
	contradicted.Diagnosis = proposal.Diagnosis{
		RootCause:     "the guest crashed and needs restarting",
		Supporting:    []proposal.EvidenceRef{{ID: "lnms-1", Claim: "the guest is not running", Cited: true}},
		Contradicting: []proposal.EvidenceRef{{ID: "pve-tasks-101", Claim: "the stop was a DELIBERATE operator task", Cited: true}},
	}
	clean := row("TG-clean")
	clean.DiagnosisRecorded = true
	clean.Diagnosis = proposal.Diagnosis{
		RootCause:  "the unit failed on boot",
		Supporting: []proposal.EvidenceRef{{ID: "svc-1", Claim: "nginx.service is failed", Cited: true}},
	}
	legacy := row("TG-legacy") // a record from before migration 0056: no diagnosis column at all

	st := newMemTriage(contradicted, clean, legacy)
	mdl := &scriptedJudge{verdicts: map[string]string{"TG-": goodVerdict}}
	acts := &Activities{D: Deps{Model: mdl, Store: st}}
	out, err := acts.JudgeBatchActivity(context.Background(), time.Now().UTC())
	if err != nil || out.Judged != 3 {
		t.Fatalf("all three sessions must be judged: %+v (err %v)", out, err)
	}

	bad, ok := st.judgments["TG-contradicted"][judge.DimDiagnosisGrounded]
	if !ok {
		t.Fatal("NO diagnosis_grounded row was written — the dimension is defined and never evaluated, so an " +
			"agent can state a root cause its own grounded evidence refutes, propose the action anyway, and " +
			"pay nothing for it (the recorded A2 failure)")
	}
	good := st.judgments["TG-clean"][judge.DimDiagnosisGrounded]
	if bad >= good {
		t.Fatalf("contradicted session scored %v and the clean one %v — the axis is not discriminating, so the "+
			"typed claim is still decoration", bad, good)
	}
	if bad != 1 {
		t.Errorf("a self-contradicted asserted root cause must land the floor 1, got %v", bad)
	}
	// N/A IS NOT A FLOOR. A pre-migration record's empty diagnosis is the SCHEMA's silence; scoring it would
	// retro-grade every historical session against a rule it was never offered (the TG-61 global-floor class).
	if _, scored := st.judgments["TG-legacy"][judge.DimDiagnosisGrounded]; scored {
		t.Error("a record from before the diagnosis column was scored on the diagnosis axis — a dimension " +
			"floored across a whole population is what fired the flywheel's Regressed trigger for every skill at once")
	}
	// The judge model must NOT have been asked for this axis: it is a fact the orchestrator bound, and a
	// model that could re-author it would have un-done the binding.
	if _, leaked := st.judgments["TG-contradicted"]["diagnosis_grounded_llm"]; leaked {
		t.Error("an LLM-authored diagnosis score reached the store")
	}
}

// TG-202 — THE ESTATE AXIS IS ACTUALLY EVALUATED, AND ONLY WHEN THE GRAPH CAN SPEAK.
//
// This is the WIRING oracle for the second deterministic dimension. `core/judge` had ZERO estate references:
// the judge scored a root cause with no access to the runs_on/depends_on structure that decides whether the
// named cause can even reach the host that alerted. A scorer nobody calls closes nothing, and a graph nobody
// hands to the judge is the same non-event.
//
// KILLING MUTATION: drop `Estate: estateHolder` at the composition root (cmd/worker/main.go) or delete the
// ScoreEstateGrounded block from JudgeBatchActivity. RED — a session blaming a hypervisor the alerting guest
// does not run on is judged, marked and pooled exactly like one that names the guest's real hypervisor, so
// TG proposes a restart on the wrong machine and no dimension anywhere moves.
func TestJudgeBatchWritesTheEstateDimensionFromTheWiredGraph(t *testing.T) {
	g := estate.NewGraph()
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeVM, Name: "web01"}, To: estate.Entity{Type: estate.TypePVENode, Name: "node-1"},
		Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeVM, Name: "db01"}, To: estate.Entity{Type: estate.TypePVENode, Name: "node-2"},
		Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})

	// Both sessions alert on web01 (row()'s host) and both propose. They differ ONLY in which machine the
	// typed claim blames — the thing an estate-blind judge cannot see.
	impossible := row("TG-impossible")
	impossible.DiagnosisRecorded = true
	impossible.Diagnosis = proposal.Diagnosis{RootCause: "node-2 exhausted its memory", Mechanism: "the guest was OOM-killed"}
	correct := row("TG-correct")
	correct.DiagnosisRecorded = true
	correct.Diagnosis = proposal.Diagnosis{RootCause: "node-1 exhausted its memory", Mechanism: "the guest was OOM-killed"}
	// A claim about a machine this graph has never heard of: the graph DOES NOT KNOW, so no row at all.
	unknown := row("TG-unknown")
	unknown.DiagnosisRecorded = true
	unknown.Diagnosis = proposal.Diagnosis{RootCause: "the upstream billing API timed out"}

	st := newMemTriage(impossible, correct, unknown)
	mdl := &scriptedJudge{verdicts: map[string]string{"TG-": goodVerdict}}
	acts := &Activities{D: Deps{Model: mdl, Store: st, Estate: estate.NewHolder(g)}}
	out, err := acts.JudgeBatchActivity(context.Background(), time.Now().UTC())
	if err != nil || out.Judged != 3 {
		t.Fatalf("all three sessions must be judged: %+v (err %v)", out, err)
	}

	bad, ok := st.judgments["TG-impossible"][judge.DimEstateGrounded]
	if !ok {
		t.Fatal("NO estate_grounded row was written — the causal graph is built, held and refreshed in this " +
			"worker and the judge never consults it, so a diagnosis blaming a hypervisor the alerting guest " +
			"does not run on is scored exactly like a correct one (TG-202: core/judge had zero estate references)")
	}
	good := st.judgments["TG-correct"][judge.DimEstateGrounded]
	if bad >= good {
		t.Fatalf("the topologically impossible claim scored %v and the correct one %v — the axis is not "+
			"discriminating, so naming the wrong machine still costs the agent nothing", bad, good)
	}
	if bad != 2 || good != 5 {
		t.Errorf("scores %v (impossible) / %v (adjacent), want 2 / 5", bad, good)
	}
	// N/A IS NOT A FLOOR — the graph not knowing an entity is not the agent being wrong.
	if v, scored := st.judgments["TG-unknown"][judge.DimEstateGrounded]; scored {
		t.Errorf("a claim naming nothing in the estate scored %v — an evaluator that reads its own blind spots "+
			"as agent error floors the dimension across the population the moment a topology source goes down", v)
	}
	// The yield register: the axis says how many sessions it actually spoke to, so "wired and producing
	// nothing" is visible in the Temporal UI instead of looking like a dimension that was never added.
	if out.EstateScored != 2 {
		t.Errorf("EstateScored=%d, want 2 — the counter is how an operator sees the axis is alive", out.EstateScored)
	}
}

// A DEPLOYMENT WITH NO ESTATE WIRED SCORES EXACTLY AS IT DID BEFORE (house rule: never change behaviour for
// existing deployments without a safe default). No graph ⇒ no estate rows, and every other dimension is
// untouched — the axis fails toward silence, never toward a floor.
func TestWithoutAnEstateTheJudgeScoresExactlyAsBefore(t *testing.T) {
	r := row("TG-nograph")
	r.DiagnosisRecorded = true
	r.Diagnosis = proposal.Diagnosis{RootCause: "node-2 exhausted its memory"}
	st := newMemTriage(r)
	mdl := &scriptedJudge{verdicts: map[string]string{"TG-": goodVerdict}}
	acts := &Activities{D: Deps{Model: mdl, Store: st}} // Estate deliberately nil
	out, err := acts.JudgeBatchActivity(context.Background(), time.Now().UTC())
	if err != nil || out.Judged != 1 {
		t.Fatalf("judged=%+v err=%v", out, err)
	}
	if v, scored := st.judgments["TG-nograph"][judge.DimEstateGrounded]; scored {
		t.Fatalf("an estate_grounded row (%v) was written with NO graph wired — a judgement about topology "+
			"invented from the absence of a topology is worse than no judgement at all", v)
	}
	if out.EstateScored != 0 {
		t.Errorf("EstateScored=%d with no graph wired", out.EstateScored)
	}
	if _, ok := st.judgments["TG-nograph"][judge.DimDiagnosisGrounded]; !ok {
		t.Error("the diagnosis axis stopped being written when no estate was wired — the two axes are independent")
	}
}
