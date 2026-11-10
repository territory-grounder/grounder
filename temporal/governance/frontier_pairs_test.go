package governance

// TG-222 / spec/004 REQ-307 — the production frontier cross-check catches the class no purely-local metric
// catches: a judge that stopped scoring while every local signal read healthy.
//
// The chain under test is the real one end to end — ModelPairSource (the production PairSource) →
// FrontierCrossCheckMonitor.Run → JudgeDeadMan over a real core/breaker → the accrual halt. The only fakes
// are the sample store and the model transport, which is where the process boundary genuinely is.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/breaker"
	coregov "github.com/territory-grounder/grounder/core/governance"
)

type fakeSample []coregov.CrossCheckRow

func (f fakeSample) RecentForCrossCheck(context.Context, int) ([]coregov.CrossCheckRow, error) {
	return []coregov.CrossCheckRow(f), nil
}

type erroringSample struct{}

func (erroringSample) RecentForCrossCheck(context.Context, int) ([]coregov.CrossCheckRow, error) {
	return nil, errors.New("sample read failed")
}

// scriptedFrontier answers every re-judgment with one canned verdict, or fails.
type scriptedFrontier struct {
	reply string
	err   error
	calls int
	tiers []string
}

func (s *scriptedFrontier) Complete(_ context.Context, _, tier string, _ []model.Message) (string, error) {
	s.calls++
	s.tiers = append(s.tiers, tier)
	return s.reply, s.err
}

const strongVerdict = `{"correct_diagnosis":5,"evidence_grounded":5,"sensible_proposal":4,"appropriate_band":5,"falsifiable_prediction":4}`
const weakVerdict = `{"correct_diagnosis":1,"evidence_grounded":1,"sensible_proposal":2,"appropriate_band":1,"falsifiable_prediction":1}`

func row(ref string, localScored bool, localMean float64) coregov.CrossCheckRow {
	return coregov.CrossCheckRow{
		ExternalRef: ref, Host: "web01", AlertRule: "HostDown", Band: "AUTO_NOTICE",
		Outcome: "proposed", Proposed: true, Op: "restart-service",
		LocalScored: localScored, LocalMean: localMean,
	}
}

// THE 3-WEEK DEAD-JUDGE CLASS: the local judge scored NOTHING while the frontier scores every session it is
// shown. Liveness alone can be evaded by a judge that writes nothing at all; the frontier proves the
// sessions were judgeable, so the local silence is a fault — and it must HALT accrual, not merely warn.
func TestFrontierConfirmsDeathAndHaltsAccrual(t *testing.T) {
	var sample fakeSample
	for _, ref := range []string{"TG-1", "TG-2", "TG-3", "TG-4"} {
		sample = append(sample, row(ref, false, 0))
	}
	fm := &scriptedFrontier{reply: strongVerdict}
	dm, err := coregov.NewJudgeDeadMan(breaker.NewMemStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	esc := &recordEsc{}
	acts := &Activities{CrossCheck: &coregov.FrontierCrossCheckMonitor{
		Pairs:      &ModelPairSource{Sample: sample, Model: fm, Tier: "frontier-tier", Limit: 10},
		Escalation: esc,
		Halt:       dm,
	}}

	res, err := acts.FrontierCrossCheckActivity(context.Background())
	if err != nil {
		t.Fatalf("cross-check activity: %v", err)
	}
	if !res.Death {
		t.Fatalf("the frontier scored 4 sessions the local judge left unscored — that is DEATH, got %+v", res)
	}
	if !res.Halted {
		t.Fatal("a CONFIRMED dead judge must halt judged accrual, not merely warn")
	}
	if halted, reason := dm.Halted(context.Background()); !halted || !strings.Contains(reason, "frontier") {
		t.Fatalf("the dead-man must be halted with a frontier-attributed reason, got halted=%v %q", halted, reason)
	}
	if len(esc.kinds) == 0 || esc.kinds[0] != "judge-death" {
		t.Fatalf("the death must also page, got %v", esc.kinds)
	}
	// The re-judgment genuinely ran on the INDEPENDENT tier, one call per sampled session.
	if fm.calls != len(sample) {
		t.Fatalf("the frontier made %d calls for %d sessions", fm.calls, len(sample))
	}
	for _, tier := range fm.tiers {
		if tier != "frontier-tier" {
			t.Fatalf("a re-judgment ran on tier %q, not the independent one", tier)
		}
	}
}

// DRIFT pages but does NOT halt: a disagreement says one of the two judges is wrong, and which one is a
// human adjudication. Halting on drift would let a bad FRONTIER model stop the flywheel.
func TestFrontierDriftWarnsWithoutHalting(t *testing.T) {
	var sample fakeSample
	for _, ref := range []string{"TG-1", "TG-2", "TG-3", "TG-4", "TG-5", "TG-6", "TG-7"} {
		sample = append(sample, row(ref, true, 4.6)) // local says "strong"
	}
	fm := &scriptedFrontier{reply: weakVerdict} // frontier says "weak" on every one
	dm, err := coregov.NewJudgeDeadMan(breaker.NewMemStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	esc := &recordEsc{}
	m := &coregov.FrontierCrossCheckMonitor{
		Pairs:      &ModelPairSource{Sample: sample, Model: fm, Tier: "frontier-tier", Limit: 10},
		Escalation: esc,
		Halt:       dm,
	}
	res, err := m.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Drift || res.Death {
		t.Fatalf("total disagreement over 7 comparable pairs is DRIFT, not death: %+v", res)
	}
	if res.Halted {
		t.Fatal("drift must not halt accrual — which judge is wrong is a human adjudication")
	}
	if halted, _ := dm.Halted(context.Background()); halted {
		t.Fatal("the dead-man must be untouched by drift")
	}
	if len(esc.kinds) != 1 || esc.kinds[0] != "judge-drift" {
		t.Fatalf("drift must page as judge-drift, got %v", esc.kinds)
	}
}

// A frontier that cannot answer can only UNDER-report death — never invent it. A broken second opinion must
// not be able to halt the flywheel on its own.
func TestAFailingFrontierNeverManufacturesDeath(t *testing.T) {
	var sample fakeSample
	for _, ref := range []string{"TG-1", "TG-2", "TG-3", "TG-4"} {
		sample = append(sample, row(ref, false, 0))
	}
	fm := &scriptedFrontier{err: errors.New("model gateway breaker_open")}
	m := &coregov.FrontierCrossCheckMonitor{
		Pairs: &ModelPairSource{Sample: sample, Model: fm, Tier: "frontier-tier", Limit: 10},
	}
	res, err := m.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Death || res.DeathHits != 0 {
		t.Fatalf("a frontier that scored nothing must not report death: %+v", res)
	}
}

// A sample the store cannot read is an ERROR, never an empty pair set: zero pairs evaluate to a clean bill
// of health, which is a false all-clear from a broken instrument.
func TestAnUnreadableSampleIsLoud(t *testing.T) {
	m := &coregov.FrontierCrossCheckMonitor{
		Pairs: &ModelPairSource{Sample: erroringSample{}, Model: &scriptedFrontier{reply: strongVerdict}, Tier: "f"},
	}
	if _, err := m.Run(context.Background()); err == nil {
		t.Fatal("an unreadable sample must surface, never read as a healthy judge")
	}
	// So is an unconfigured source.
	if _, err := (&ModelPairSource{}).RecentCrossCheckPairs(context.Background()); err == nil {
		t.Fatal("an unconfigured pair source must refuse, not return zero pairs")
	}
}

// A cross-check on the LOCAL judge's own tier is the judge grading itself — it reintroduces the exact blind
// spot the anchor exists to close, so the configuration is refusable by construction.
func TestIndependenceIsCheckable(t *testing.T) {
	src := &ModelPairSource{Tier: "judge-tier"}
	if src.IndependentOf("judge-tier") {
		t.Fatal("a source on the local judge's own tier is not independent")
	}
	if !src.IndependentOf("some-other-tier") {
		t.Fatal("a distinct tier is independent")
	}
	if (&ModelPairSource{}).IndependentOf("judge-tier") {
		t.Fatal("an unconfigured tier is not independent")
	}
}

type recordEsc struct{ kinds []string }

func (r *recordEsc) Warn(_ context.Context, kind, _ string) error {
	r.kinds = append(r.kinds, kind)
	return nil
}
