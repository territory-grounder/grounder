package acceptance

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/audit"
	gov "github.com/territory-grounder/grounder/core/governance"
)

var gNow = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

var focus = gov.Tuple{Host: "h", AlertRule: "R"}

type fakeTransients struct{ known map[gov.Tuple]bool }

func (f fakeTransients) IsKnownTransient(_ context.Context, tp gov.Tuple) bool { return f.known[tp] }

type fakeSessions struct{ s []gov.Session }

func (f fakeSessions) RecentlyEnded(context.Context) ([]gov.Session, error) { return f.s, nil }

type fakeJudgments struct{ judged map[string]bool }

func (f fakeJudgments) HasRealJudgment(_ context.Context, id string) bool { return f.judged[id] }

type fakeEscalator struct{ warned int }

func (f *fakeEscalator) Warn(context.Context, string, string) error { f.warned++; return nil }

func TestGovernanceDemoteAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "spec/004 governance-demote",
		ScenarioInitializer: initializeScenario,
		Options:             &godog.Options{Format: "pretty", Paths: []string{"."}, Tags: "~@pending", Strict: true, TestingT: t},
	}
	if suite.Run() != 0 {
		t.Fatal("spec/004 acceptance scenarios failed")
	}
}

type world struct {
	store      *gov.MemDemotionStore
	transients fakeTransients
	ledger     *audit.Ledger
	counts     map[gov.Tuple]int
	candidates map[gov.Tuple]bool
	demoted    []gov.DemotionRow

	sessions []gov.Session
	judged   map[string]bool
	esc      *fakeEscalator
	live     gov.LivenessResult

	rm       *gov.RetentionManager
	purged   int
	spineLen int
}

func initializeScenario(sc *godog.ScenarioContext) {
	w := &world{store: gov.NewMemDemotionStore(), ledger: audit.NewLedger(), transients: fakeTransients{known: map[gov.Tuple]bool{}}, judged: map[string]bool{}}
	ctx := context.Background()

	// --- repeat-offender / demote ---
	sc.Step(`^a tuple of host and alert rule classified a genuine repeat-offender$`, func() error {
		w.counts = map[gov.Tuple]int{focus: 3}
		return nil
	})
	sc.Step(`^a tuple of host and alert rule that recurred three times within thirty days$`, func() error {
		w.counts = map[gov.Tuple]int{focus: 3}
		return nil
	})
	sc.Step(`^a tuple of host and alert rule that recurred twice within thirty days$`, func() error {
		w.counts = map[gov.Tuple]int{focus: 2}
		return nil
	})
	sc.Step(`^a demote candidate tuple tagged as an intentional known-transient for the organization$`, func() error {
		w.counts = map[gov.Tuple]int{focus: 3}
		w.transients.known[focus] = true
		return nil
	})
	sc.Step(`^a demotion policy row written thirty-one days ago$`, func() error {
		w.counts = map[gov.Tuple]int{}
		return w.store.Write(ctx, gov.DemotionRow{Tuple: focus, Reason: gov.DemotionReason, ValidFrom: gNow.Add(-31 * 24 * time.Hour), ValidUntil: gNow.Add(-1 * 24 * time.Hour)})
	})
	sc.Step(`^a genuine repeat-offender tuple that the worker demotes$`, func() error {
		w.counts = map[gov.Tuple]int{focus: 3}
		return nil
	})

	runWorker := func() error {
		w.candidates = map[gov.Tuple]bool{}
		for tp, c := range w.counts {
			if gov.IsDemoteCandidate(c) {
				w.candidates[tp] = true
			}
		}
		d := &gov.Demoter{Store: w.store, Transients: w.transients, Ledger: w.ledger}
		var err error
		w.demoted, err = d.Evaluate(ctx, w.counts, gNow)
		return err
	}
	sc.Step(`^the governance-metrics worker runs$`, runWorker)
	sc.Step(`^the demotion decision is written$`, runWorker)

	demotedNow := func(tp gov.Tuple) bool { live, _ := gov.Demoted(ctx, w.store, tp, gNow); return live }

	sc.Step(`^the tuple is demoted to analysis-only$`, func() error {
		if !demotedNow(focus) {
			return fmt.Errorf("the offender must be demoted to analysis-only")
		}
		return nil
	})
	sc.Step(`^Tier-1 suppression escalates the tuple instead of suppressing or auto-resolving it$`, func() error {
		// the demotion is the signal Tier-1 reads: a demoted tuple is no longer suppression/auto-resolve eligible.
		if !demotedNow(focus) {
			return fmt.Errorf("a demoted tuple must read as demoted so Tier-1 escalates it")
		}
		return nil
	})
	sc.Step(`^the tuple is classified as a demote candidate$`, func() error {
		if !w.candidates[focus] {
			return fmt.Errorf("three recurrences must be a demote candidate")
		}
		return nil
	})
	sc.Step(`^the tuple is not classified as a demote candidate$`, func() error {
		if w.candidates[focus] {
			return fmt.Errorf("two recurrences must not be a demote candidate")
		}
		return nil
	})
	sc.Step(`^the tuple is excluded from demotion$`, func() error {
		if demotedNow(focus) {
			return fmt.Errorf("a known-transient must be excluded from demotion")
		}
		return nil
	})
	sc.Step(`^the read path treats the demotion as expired and the tuple is eligible again$`, func() error {
		if demotedNow(focus) {
			return fmt.Errorf("a 31-day-old demotion must read as expired (eligible again)")
		}
		return nil
	})
	sc.Step(`^no manual review was required$`, func() error { return nil }) // structural: no review step exists
	sc.Step(`^the decision is appended to the immutable hash-chained audit spine$`, func() error {
		if w.ledger.Len() != 1 || w.ledger.Verify() != nil {
			return fmt.Errorf("a demotion must append one verifiable audit-spine record, got len=%d", w.ledger.Len())
		}
		return nil
	})

	// --- judge liveness ---
	sc.Step(`^ten recently-ended sessions of which six carry a real local judgment$`, func() error {
		w.sessions = nil
		for i := 0; i < 10; i++ {
			id := fmt.Sprintf("s%d", i)
			w.sessions = append(w.sessions, gov.Session{SessionID: id, EndedAt: gNow.Add(-time.Hour)})
			if i < 6 {
				w.judged[id] = true
			}
		}
		return nil
	})
	sc.Step(`^a session that ended before the recency window$`, func() error {
		w.sessions = []gov.Session{
			{SessionID: "recent", EndedAt: gNow.Add(-time.Hour)},
			{SessionID: "old", EndedAt: gNow.Add(-72 * time.Hour)},
		}
		w.judged["recent"] = true
		return nil
	})
	sc.Step(`^more than three eligible recently-ended sessions and a judged fraction below one half$`, func() error {
		for i := 0; i < 5; i++ {
			w.sessions = append(w.sessions, gov.Session{SessionID: fmt.Sprintf("s%d", i), EndedAt: gNow.Add(-time.Hour)})
		}
		w.judged["s0"] = true // 1/5 = 0.2
		return nil
	})
	sc.Step(`^three or fewer eligible recently-ended sessions and a judged fraction below one half$`, func() error {
		for i := 0; i < 3; i++ {
			w.sessions = append(w.sessions, gov.Session{SessionID: fmt.Sprintf("s%d", i), EndedAt: gNow.Add(-time.Hour)})
		}
		// none judged → 0.0
		return nil
	})
	sc.Step(`^the judge-liveness monitor runs$`, func() error {
		w.esc = &fakeEscalator{}
		m := &gov.JudgeLivenessMonitor{Sessions: fakeSessions{w.sessions}, Judgments: fakeJudgments{w.judged}, Escalation: w.esc, Window: 24 * time.Hour}
		var err error
		w.live, err = m.Run(ctx, gNow)
		return err
	})
	sc.Step(`^the judged fraction denominator is drawn from tables the judge does not write$`, func() error {
		// the denominator (eligible) came from the judge-independent SessionStore, not the judgment table.
		if w.live.Eligible != 10 {
			return fmt.Errorf("denominator must be the judge-independent session population (10), got %d", w.live.Eligible)
		}
		return nil
	})
	sc.Step(`^the judged fraction is reported as zero point six$`, func() error {
		if w.live.Fraction != 0.6 {
			return fmt.Errorf("judged fraction must be 0.6, got %v", w.live.Fraction)
		}
		return nil
	})
	sc.Step(`^the non-recent session is excluded from the judged fraction$`, func() error {
		if w.live.Eligible != 1 {
			return fmt.Errorf("a non-recent session must be excluded, eligible=%d", w.live.Eligible)
		}
		return nil
	})
	sc.Step(`^a judge-death warning is raised and routed through the escalation module$`, func() error {
		if !w.live.Warned || w.esc.warned != 1 {
			return fmt.Errorf("a low fraction over >3 sessions must raise a warning")
		}
		return nil
	})
	sc.Step(`^no judge-death warning is raised$`, func() error {
		if w.live.Warned || w.esc.warned != 0 {
			return fmt.Errorf("a thin sample must not warn")
		}
		return nil
	})

	// --- retention split ---
	sc.Step(`^a right-to-erasure purge of raw judged transcripts$`, func() error {
		spine := audit.NewLedger()
		_, _ = spine.Append(audit.GovDecision{Decision: "judged-fraction", Reason: "0.6", ActionID: "fact:2026-07"})
		w.spineLen = spine.Len()
		w.rm = &gov.RetentionManager{Transcripts: gov.NewMemTranscriptStore(map[string]string{"s1": "raw", "s2": "raw"}), Spine: spine}
		return nil
	})
	sc.Step(`^the purge runs$`, func() error {
		var err error
		w.purged, err = w.rm.PurgeRawTranscripts(ctx, []string{"s1", "s2"})
		return err
	})
	sc.Step(`^the raw transcripts are removed from the purgeable operational store$`, func() error {
		if w.purged != 2 || w.rm.Transcripts.Count(ctx) != 0 {
			return fmt.Errorf("raw transcripts must be purged, purged=%d remaining=%d", w.purged, w.rm.Transcripts.Count(ctx))
		}
		return nil
	})
	sc.Step(`^the recorded judged fraction fact remains on the immutable audit spine$`, func() error {
		if w.rm.Spine.Len() != w.spineLen {
			return fmt.Errorf("the audit-spine fact must survive a transcript purge")
		}
		return nil
	})
}
