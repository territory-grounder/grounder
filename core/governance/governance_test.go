package governance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

var gNow = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

func inc(host, rule, ref string, ago time.Duration) Incident {
	return Incident{Tuple: Tuple{Host: host, AlertRule: rule}, ExternalRef: ref, ClosedAt: gNow.Add(-ago)}
}

func TestCountByTupleCountsIncidentsNotEvents(t *testing.T) {
	// three DISTINCT incidents for one tuple, plus a duplicate event of TG-1 (same ref) → counts 3.
	incs := []Incident{
		inc("h", "R", "TG-1", time.Hour), inc("h", "R", "TG-1", 30*time.Minute), // same incident twice
		inc("h", "R", "TG-2", 2*time.Hour), inc("h", "R", "TG-3", 3*time.Hour),
		inc("h", "R", "TG-OLD", 40*24*time.Hour), // outside the 30d window
	}
	counts := CountByTuple(incs, gNow)
	if counts[Tuple{"h", "R"}] != 3 {
		t.Fatalf("must count 3 distinct in-window incidents, got %d", counts[Tuple{"h", "R"}])
	}
}

func TestIsDemoteCandidate(t *testing.T) {
	if !IsDemoteCandidate(3) || IsDemoteCandidate(2) {
		t.Fatal("threshold is >=3 incidents")
	}
}

type fakeTransients struct{ known map[Tuple]bool }

func (f fakeTransients) IsKnownTransient(_ context.Context, tp Tuple) bool { return f.known[tp] }

func TestDemoterEvaluate(t *testing.T) {
	l := audit.NewLedger()
	store := NewMemDemotionStore()
	d := &Demoter{Store: store, Transients: fakeTransients{known: map[Tuple]bool{{"h", "Benign"}: true}}, Ledger: l}
	counts := map[Tuple]int{
		{"h", "R"}:      3, // demote
		{"h", "Twice"}:  2, // not a candidate
		{"h", "Benign"}: 5, // known-transient → excluded
	}
	rows, err := d.Evaluate(context.Background(), counts, gNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Tuple != (Tuple{"h", "R"}) {
		t.Fatalf("only the genuine offender must be demoted, got %+v", rows)
	}
	if live, _ := Demoted(context.Background(), store, Tuple{"h", "R"}, gNow); !live {
		t.Fatal("demoted tuple must read as live")
	}
	if live, _ := Demoted(context.Background(), store, Tuple{"h", "Benign"}, gNow); live {
		t.Fatal("a known-transient must not be demoted")
	}
	if l.Len() != 1 || l.Verify() != nil {
		t.Fatalf("one ledger record per demotion, verifiable; got len=%d", l.Len())
	}
}

func TestDemotionAutoExpires(t *testing.T) {
	store := NewMemDemotionStore()
	_ = store.Write(context.Background(), DemotionRow{Tuple: Tuple{"h", "R"}, Reason: DemotionReason, ValidFrom: gNow.Add(-31 * 24 * time.Hour), ValidUntil: gNow.Add(-1 * 24 * time.Hour)})
	// 31 days old, expired 1 day ago → the read path treats it as absent (eligible again).
	if live, _ := Demoted(context.Background(), store, Tuple{"h", "R"}, gNow); live {
		t.Fatal("an expired demotion must not read as live (auto-expiry)")
	}
}

// --- judge liveness ---

type fakeSessions struct{ s []Session }

func (f fakeSessions) RecentlyEnded(context.Context) ([]Session, error) { return f.s, nil }

type fakeJudgments struct{ judged map[string]bool }

func (f fakeJudgments) HasRealJudgment(_ context.Context, id string) bool { return f.judged[id] }

type fakeEscalator struct{ warned int }

func (f *fakeEscalator) Warn(context.Context, string, string) error { f.warned++; return nil }

func sessions(n int, endedAgo time.Duration) []Session {
	var out []Session
	for i := 0; i < n; i++ {
		out = append(out, Session{SessionID: string(rune('a' + i)), EndedAt: gNow.Add(-endedAgo)})
	}
	return out
}

func TestJudgeLivenessFraction(t *testing.T) {
	ss := sessions(10, time.Hour)
	judged := map[string]bool{}
	for i := 0; i < 6; i++ {
		judged[string(rune('a'+i))] = true // 6 of 10 judged
	}
	esc := &fakeEscalator{}
	m := &JudgeLivenessMonitor{Sessions: fakeSessions{ss}, Judgments: fakeJudgments{judged}, Escalation: esc, Window: 24 * time.Hour}
	res, _ := m.Run(context.Background(), gNow)
	if res.Eligible != 10 || res.Judged != 6 || res.Fraction != 0.6 {
		t.Fatalf("judged fraction must be 6/10=0.6, got %+v", res)
	}
	if res.Warned || esc.warned != 0 {
		t.Fatal("0.6 is above the death threshold — no warning")
	}
}

func TestJudgeDeathWarning(t *testing.T) {
	// >3 eligible, fraction below 0.5 → warn
	ss := sessions(5, time.Hour)
	esc := &fakeEscalator{}
	m := &JudgeLivenessMonitor{Sessions: fakeSessions{ss}, Judgments: fakeJudgments{map[string]bool{"a": true}}, Escalation: esc, Window: 24 * time.Hour}
	res, _ := m.Run(context.Background(), gNow)
	if !res.Warned || esc.warned != 1 {
		t.Fatalf("5 eligible at 0.2 must warn, got %+v", res)
	}
	// <=3 eligible, same low fraction → no warn (sample too thin)
	ss3 := sessions(3, time.Hour)
	esc2 := &fakeEscalator{}
	m2 := &JudgeLivenessMonitor{Sessions: fakeSessions{ss3}, Judgments: fakeJudgments{map[string]bool{}}, Escalation: esc2, Window: 24 * time.Hour}
	if r, _ := m2.Run(context.Background(), gNow); r.Warned || esc2.warned != 0 {
		t.Fatalf("3-or-fewer eligible must not warn, got %+v", r)
	}
}

func TestJudgeLivenessRecencyExclusion(t *testing.T) {
	// a session ended long before the window is excluded from the eligible population.
	ss := []Session{{SessionID: "recent", EndedAt: gNow.Add(-time.Hour)}, {SessionID: "old", EndedAt: gNow.Add(-72 * time.Hour)}}
	m := &JudgeLivenessMonitor{Sessions: fakeSessions{ss}, Judgments: fakeJudgments{map[string]bool{"recent": true}}, Window: 24 * time.Hour}
	res, _ := m.Run(context.Background(), gNow)
	if res.Eligible != 1 {
		t.Fatalf("a non-recent session must be excluded from the denominator, got eligible=%d", res.Eligible)
	}
}

func TestRetentionSplit(t *testing.T) {
	spine := audit.NewLedger()
	_, _ = spine.Append(audit.GovDecision{Decision: "judged-fraction", Reason: "0.6", ActionID: "fact:2026-07"})
	before := spine.Len()
	rm := &RetentionManager{Transcripts: NewMemTranscriptStore(map[string]string{"s1": "raw", "s2": "raw"}), Spine: spine}
	n, err := rm.PurgeRawTranscripts(context.Background(), []string{"s1", "s2"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || rm.Transcripts.Count(context.Background()) != 0 {
		t.Fatalf("raw transcripts must be purged, got n=%d remaining=%d", n, rm.Transcripts.Count(context.Background()))
	}
	if spine.Len() != before {
		t.Fatal("the audit spine must be untouched by a transcript purge")
	}
}

// permitAuthority permits exactly the roles in its set; anything else is denied (fail closed).
type permitAuthority struct{ roles map[string]bool }

func (a permitAuthority) MayRestamp(role, _ string) bool { return a.roles[role] }

// AuthorizeRestamp is a fail-closed authorization gate: EVERY guard boundary must reject with
// ErrUnauthorizedRestamp and write NOTHING to the ledger; only a fully-authorized approval appends
// exactly one verifiable record. This pins each guard individually so a future refactor that drops one
// cannot pass while the two acceptance-path scenarios still go green (REQ-703, INV-19/22).
func TestAuthorizeRestampFailClosed(t *testing.T) {
	authz := permitAuthority{roles: map[string]bool{"spec-owner": true}}
	good := func() RestampApproval {
		return RestampApproval{
			ActorRole:    "spec-owner",
			OwningSpec:   "007-spec-code-lockstep",
			ChangedPaths: []string{"core/governance/lockstep_restamp.go"},
			SpecUpdated:  true,
		}
	}
	mut := func(f func(*RestampApproval)) RestampApproval { a := good(); f(&a); return a }

	cases := []struct {
		name      string
		authz     RestampAuthority
		appr      RestampApproval
		nilLedger bool
	}{
		{name: "nil ledger", authz: authz, appr: good(), nilLedger: true},
		{name: "nil authority", authz: nil, appr: good()},
		{name: "empty owning spec", authz: authz, appr: mut(func(a *RestampApproval) { a.OwningSpec = "" })},
		{name: "empty changed paths", authz: authz, appr: mut(func(a *RestampApproval) { a.ChangedPaths = nil })},
		{name: "spec not updated", authz: authz, appr: mut(func(a *RestampApproval) { a.SpecUpdated = false })},
		{name: "role not permitted", authz: authz, appr: mut(func(a *RestampApproval) { a.ActorRole = "intruder" })},
	}
	for _, c := range cases {
		var l *audit.Ledger
		if !c.nilLedger {
			l = audit.NewLedger()
		}
		err := AuthorizeRestamp(c.authz, c.appr, l)
		if !errors.Is(err, ErrUnauthorizedRestamp) {
			t.Fatalf("%s: must reject with ErrUnauthorizedRestamp, got %v", c.name, err)
		}
		if l != nil && l.Len() != 0 {
			t.Fatalf("%s: a rejected re-stamp must leave NO ledger record, got %d", c.name, l.Len())
		}
	}

	// A fully-authorized approval appends exactly one verifiable record.
	l := audit.NewLedger()
	if err := AuthorizeRestamp(authz, good(), l); err != nil {
		t.Fatalf("a fully-authorized approval must be accepted, got %v", err)
	}
	if l.Len() != 1 || l.Verify() != nil {
		t.Fatalf("an authorized re-stamp must append exactly one verifiable record, len=%d verify=%v", l.Len(), l.Verify())
	}

	// The action id is content-addressed: stable for the same (spec, paths), distinct for different paths.
	id := restampActionID(good())
	diff := mut(func(a *RestampApproval) { a.ChangedPaths = []string{"core/governance/demote.go"} })
	if restampActionID(good()) != id {
		t.Fatal("restampActionID must be stable for the same input")
	}
	if restampActionID(diff) == id {
		t.Fatal("restampActionID must differ when the changed paths differ (content-addressed, INV-07)")
	}
}

// The lag lower bound prevents just-ended, not-yet-judgeable sessions from false-paging a healthy judge (P1-15).
func TestJudgeLivenessLagLowerBound(t *testing.T) {
	var ss []Session
	judged := map[string]bool{}
	for i := 0; i < 10; i++ { // old sessions, all judged
		id := string(rune('a' + i))
		ss = append(ss, Session{SessionID: id, EndedAt: gNow.Add(-6 * time.Hour)})
		judged[id] = true
	}
	for i := 0; i < 15; i++ { // just ended (-30m), not yet judged
		ss = append(ss, Session{SessionID: "r" + string(rune('0'+i)), EndedAt: gNow.Add(-30 * time.Minute)})
	}
	// with the lag, the recent unjudged sessions are excluded → healthy judge, no page
	m := &JudgeLivenessMonitor{Sessions: fakeSessions{ss}, Judgments: fakeJudgments{judged}, Escalation: &fakeEscalator{}, Window: 24 * time.Hour, Lag: 2 * time.Hour}
	res, _ := m.Run(context.Background(), gNow)
	if res.Warned {
		t.Fatalf("just-ended un-judged sessions must not trip judge-death (lag), fraction=%v eligible=%d", res.Fraction, res.Eligible)
	}
	if res.Eligible != 10 || res.Fraction != 1.0 {
		t.Fatalf("only the lagged-enough (old) sessions are eligible and all judged, got eligible=%d fraction=%v", res.Eligible, res.Fraction)
	}
	// without the lag, the SAME population false-pages (25 eligible, 10 judged → 0.4 < 0.5)
	m2 := &JudgeLivenessMonitor{Sessions: fakeSessions{ss}, Judgments: fakeJudgments{judged}, Escalation: &fakeEscalator{}, Window: 24 * time.Hour}
	if res2, _ := m2.Run(context.Background(), gNow); !res2.Warned {
		t.Fatalf("without the lag the recent unjudged sessions should false-page, fraction=%v", res2.Fraction)
	}
}

// fakePairs is an injected PairSource for the frontier cross-check oracle.
type fakePairs struct{ pairs []CrossCheckPair }

func (f fakePairs) RecentCrossCheckPairs(context.Context) ([]CrossCheckPair, error) { return f.pairs, nil }

// The frontier cross-check catches DEATH (frontier scores sessions the local judge left unscored) even when
// the local judge still writes rows — the class judge-liveness alone can miss (P1-15).
func TestFrontierCrossCheckDetectsDeath(t *testing.T) {
	// the local judge returns unscored (-1) for every session; the frontier scores them all real.
	var pairs []CrossCheckPair
	for i := 0; i < 6; i++ {
		pairs = append(pairs, CrossCheckPair{SessionID: "s", LocalScored: false, FrontierScored: true, FrontierVerdict: "match"})
	}
	esc := &fakeEscalator{}
	m := &FrontierCrossCheckMonitor{Pairs: fakePairs{pairs}, Escalation: esc}
	res, err := m.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Death || res.DeathHits != 6 || !res.Warned {
		t.Fatalf("a frontier that scores every locally-unscored session must raise DEATH: %+v", res)
	}
	if esc.warned == 0 {
		t.Fatal("a DEATH must route a warning through escalation")
	}
}

// It catches DRIFT: the local judge keeps scoring (liveness looks healthy) but disagrees with the frontier.
// DRIFT gates on the TOTAL sample exceeding the floor (predecessor `judge_frontier_pairs > 5`, i.e. >= 6).
func TestFrontierCrossCheckDetectsDrift(t *testing.T) {
	var pairs []CrossCheckPair
	// 6 comparable pairs (over the >5 floor), only 1 agrees → agreement ≈0.17, below the 0.6 floor.
	for i := 0; i < 6; i++ {
		lv, fv := "deviation", "match"
		if i == 0 {
			lv, fv = "match", "match"
		}
		pairs = append(pairs, CrossCheckPair{SessionID: "s", LocalScored: true, LocalVerdict: lv, FrontierScored: true, FrontierVerdict: fv})
	}
	res := (&FrontierCrossCheckMonitor{}).Evaluate(pairs)
	if !res.Drift || res.Comparable != 6 || res.Agree != 1 {
		t.Fatalf("low local↔frontier agreement over >5 pairs must raise DRIFT: %+v", res)
	}
	if res.Death {
		t.Fatalf("no unscored-local pairs → no DEATH: %+v", res)
	}
	// A 5-comparable disagreeing sample is BELOW the >5 drift floor → no DRIFT (too noisy to page).
	thinDrift := pairs[:5]
	if res := (&FrontierCrossCheckMonitor{}).Evaluate(thinDrift); res.Drift {
		t.Fatalf("a 5-pair (sub-floor) disagreeing sample must not page DRIFT: %+v", res)
	}
}

// A healthy judge (full agreement) raises neither. DEATH has NO sample gate: a thin all-dead window pages
// (the predecessor's standalone unscored-rate term), while an all-dead window never raises DRIFT (no
// comparable pair → no meaningful agreement rate).
func TestFrontierCrossCheckHealthyAndThinDeath(t *testing.T) {
	var healthy []CrossCheckPair
	for i := 0; i < 6; i++ {
		healthy = append(healthy, CrossCheckPair{SessionID: "s", LocalScored: true, LocalVerdict: "match", FrontierScored: true, FrontierVerdict: "match"})
	}
	if res := (&FrontierCrossCheckMonitor{}).Evaluate(healthy); res.Drift || res.Death {
		t.Fatalf("a fully-agreeing judge must be healthy: %+v", res)
	}
	// two death-shaped pairs (DeathFraction 1.0) → DEATH now fires even on a thin window (was a false negative).
	thin := []CrossCheckPair{
		{LocalScored: false, FrontierScored: true, FrontierVerdict: "match"},
		{LocalScored: false, FrontierScored: true, FrontierVerdict: "match"},
	}
	res := (&FrontierCrossCheckMonitor{}).Evaluate(thin)
	if !res.Death {
		t.Fatalf("a thin all-dead window must page DEATH (no sample gate): %+v", res)
	}
	if res.Drift {
		t.Fatalf("an all-dead window has no comparable pair → must not raise DRIFT: %+v", res)
	}
	// STRICT boundary: DeathFraction EXACTLY 0.5 (2 dead + 2 comparable-agree) must NOT fire — the predecessor
	// uses `> 0.5`, not `>=`.
	boundary := []CrossCheckPair{
		{LocalScored: false, FrontierScored: true, FrontierVerdict: "match"},
		{LocalScored: false, FrontierScored: true, FrontierVerdict: "match"},
		{LocalScored: true, LocalVerdict: "match", FrontierScored: true, FrontierVerdict: "match"},
		{LocalScored: true, LocalVerdict: "match", FrontierScored: true, FrontierVerdict: "match"},
	}
	if res := (&FrontierCrossCheckMonitor{}).Evaluate(boundary); res.Death {
		t.Fatalf("DeathFraction exactly 0.5 must NOT page DEATH (strict >): %+v", res)
	}
}
