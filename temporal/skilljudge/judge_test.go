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
	"github.com/territory-grounder/grounder/core/judge"
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

func (m *memTriage) WriteJudgment(_ context.Context, ref, dim string, score float64, _ string) error {
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
