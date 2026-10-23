package skillgen

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// pairwiseGateway scripts the two kinds of call the gate makes. Triage single-shots answer by BODY (the candidate
// body proposes with a prediction, the production body stops), so the two deliverables differ; pairwise readings
// are answered from a per-incident script keyed by the incident's host, in reading order. Every call's tier is
// recorded.
type pairwiseGateway struct {
	mu          sync.Mutex
	readings    map[string][]string // host → replies for reading 1, reading 2
	tiers       map[string][]string // user tag → tiers seen
	fail        string              // host whose pairwise reading fails on the transport
	slow        time.Duration       // per-call latency, so concurrency is observable
	ignoreCtx   bool                // a gateway that does NOT honour cancellation — only the gate's own check can
	inflight    int
	maxInflight int
}

func (g *pairwiseGateway) Complete(ctx context.Context, user, tier string, msgs []model.Message) (string, error) {
	if err := ctx.Err(); err != nil && !g.ignoreCtx {
		return "", err
	}
	g.mu.Lock()
	g.inflight++
	if g.inflight > g.maxInflight {
		g.maxInflight = g.inflight
	}
	g.mu.Unlock()
	if g.slow > 0 {
		time.Sleep(g.slow)
	}
	g.mu.Lock()
	g.inflight--
	defer g.mu.Unlock()
	if g.tiers == nil {
		g.tiers = map[string][]string{}
	}
	g.tiers[user] = append(g.tiers[user], tier)
	prompt := msgs[0].Content
	switch user {
	case "flywheel-offline-triage":
		if strings.Contains(prompt, "CANDIDATE-BODY") {
			return `{"diagnosis":"logs unrotated","propose_action":true,"proposed_op":"rotate-logs","evidence":["disk"],"prediction":"usage falls below 80% in 10m","conclusion":"rotate"}`, nil
		}
		return `{"diagnosis":"disk full","propose_action":false,"proposed_op":"","evidence":["disk"],"prediction":"","conclusion":"escalate"}`, nil
	case judge.PairUserTag:
		host := between(prompt, `host="`, `"`)
		if host == g.fail {
			return "", errors.New("gateway down")
		}
		rs := g.readings[host]
		if len(rs) == 0 {
			return "", errors.New("no scripted reading for " + host)
		}
		g.readings[host] = rs[1:]
		return readBlind(prompt, rs[0]), nil
	}
	return "", errors.New("unexpected user tag " + user)
}

func between(s, open, close string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// A scripted reading names WHAT the fake judge prefers, not a position: "cand:<n>" picks the deliverable that
// PROPOSED (the candidate body's), "prod:<n>" the one that stopped, wherever each stands in the prompt — so a
// gate that confused its sides is caught; "pos:<n>" always answers A (a position-following judge).
func pick(what string, decided int) string { return what + ":" + string(rune('0'+decided)) }

func readBlind(prompt, script string) string {
	what, n, _ := strings.Cut(script, ":")
	a := between(prompt, "=== DELIVERABLE A ===", "=== END A ===")
	aProposed := strings.Contains(a, "proposed=true")
	choice := "A"
	switch what {
	case "cand":
		if !aProposed {
			choice = "B"
		}
	case "prod":
		if aProposed {
			choice = "B"
		}
	}
	return "<why>because</why>\nDECIDED_BY: " + n + "\nCHOICE: " + choice + "\n"
}

type fakeIncidents struct{ rows []judge.TriageRow }

func (f fakeIncidents) RecentJudgedByVersion(context.Context, int64, time.Duration, int) ([]judge.TriageRow, error) {
	return f.rows, nil
}

func pairwiseRunner(t *testing.T, g *pairwiseGateway, hosts ...string) (OfflineRunner, skillstore.Version) {
	t.Helper()
	st := skillstore.NewMemStore()
	st.PutSkill(skillstore.Skill{Name: "triage-protocol", Kind: "behavioral", Position: 5})
	prod, err := st.CreateVersion(context.Background(), skillstore.Version{SkillName: "triage-protocol", Version: "1.0.0", Body: "PRODUCTION-BODY", ContentHash: skillstore.ContentHash("PRODUCTION-BODY", skillstore.Version{}.AppliesWhen), Author: "t", Source: "seed", Rationale: "r"})
	if err != nil {
		t.Fatal(err)
	}
	lg := audit.NewLedger()
	if _, err := skillstore.Transition(context.Background(), st, lg, prod.ID, skillstore.StatusTrial, "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := skillstore.Transition(context.Background(), st, lg, prod.ID, skillstore.StatusProduction, "t"); err != nil {
		t.Fatal(err)
	}
	var rows []judge.TriageRow
	for i, h := range hosts {
		rows = append(rows, judge.TriageRow{ExternalRef: "inc-" + h, Host: h, AlertRule: "Disk full", Band: "POLL_PAUSE", Outcome: "proposed", CreatedAt: time.Now().Add(-time.Duration(i) * time.Hour)})
	}
	r := OfflineRunner{Model: g, Store: st, Incidents: fakeIncidents{rows}, Cfg: OfflineConfig{Window: 14 * 24 * time.Hour, DiscoveryLimit: 6, MinIncidents: 3, Model: "fast"}}
	cand := skillstore.Version{SkillName: "triage-protocol", Version: "1.0.0-cand1", Body: "CANDIDATE-BODY"}
	return r, cand
}

// The gate's shape: both arms triaged on the fast tier, the pair judged twice on the canonical judge tier, the
// candidate on the LEFT; a candidate preferred consistently on every incident passes with net +1 and the verdicts
// stored on the result; the trigger dimension names itself in the detail but never reaches the judge.
func TestOfflinePairwiseGateAdmitsAConsistentWinner(t *testing.T) {
	g := &pairwiseGateway{readings: map[string][]string{
		"h1": {pick("cand", 3), pick("cand", 3)}, // the proposing deliverable, wherever it stands → the candidate both times
		"h2": {pick("cand", 5), pick("cand", 5)},
		"h3": {pick("cand", 3), pick("cand", 4)},
	}}
	r, cand := pairwiseRunner(t, g, "h1", "h2", "h3")
	res, err := r.RunOffline(context.Background(), cand, "falsifiable_prediction")
	if err != nil {
		t.Fatal(err)
	}
	if !res.RegressionPass || res.DiscoveryDelta != 1 || len(res.Pairwise) != 3 {
		t.Fatalf("a consistent winner on every incident: %+v", res)
	}
	if !strings.Contains(res.Detail, "W3/L0/T0") || !strings.Contains(res.Detail, "falsifiable_prediction") {
		t.Fatalf("detail must carry the tally and the trigger dimension: %s", res.Detail)
	}
	for _, tier := range g.tiers["flywheel-offline-triage"] {
		if tier != "fast" {
			t.Fatalf("triage single-shots run on the configured fast tier, got %q", tier)
		}
	}
	if n := len(g.tiers[judge.PairUserTag]); n != 6 {
		t.Fatalf("two pairwise readings per incident: got %d calls", n)
	}
	for _, tier := range g.tiers[judge.PairUserTag] {
		if tier != judge.DefaultParams().Model {
			t.Fatalf("the pairwise judge runs on the canonical judge tier %q, got %q", judge.DefaultParams().Model, tier)
		}
	}
	if res.Pairwise[0].Winner != "left" || res.Pairwise[0].Ref != "inc-h1" {
		t.Fatalf("verdicts land in incident order with the candidate as left: %+v", res.Pairwise[0])
	}
	// The production deliverable preferred on every incident (one of them on BAND) → net −1 and a regression; a
	// position-following judge → ties.
	g2 := &pairwiseGateway{readings: map[string][]string{
		"h1": {pick("prod", 2), pick("prod", 2)},
		"h2": {pick("prod", 3), pick("prod", 3)},
		"h3": {pick("pos", 3), pick("pos", 3)},
	}}
	r2, cand2 := pairwiseRunner(t, g2, "h1", "h2", "h3")
	res2, err := r2.RunOffline(context.Background(), cand2, "appropriate_band")
	if err != nil {
		t.Fatal(err)
	}
	if res2.RegressionPass || res2.DiscoveryDelta > -0.66 || !strings.Contains(res2.Detail, "W0/L2/T1") || !strings.Contains(res2.Detail, "position-dependent 1") || !res2.Pairwise[2].PositionDependent || res2.Refusal == "" {
		t.Fatalf("losses and a positional tie must be read as such: %+v", res2)
	}
	if !strings.Contains(res.Detail, "3/3 incidents judged cleanly") {
		t.Fatalf("the success path states its coverage: %s", res.Detail)
	}
}

// The fan-out honours its concurrency bound and a cancelled context: no more than Concurrency incidents are in flight
// at once, and a context cancelled before the run fails closed with the cancellation surfaced.
func TestOfflinePairwiseGateHonoursConcurrencyAndCancellation(t *testing.T) {
	g := &pairwiseGateway{readings: map[string][]string{}, slow: 15 * time.Millisecond}
	hosts := []string{"h1", "h2", "h3", "h4", "h5", "h6"}
	for _, h := range hosts {
		g.readings[h] = []string{pick("cand", 3), pick("cand", 3)}
	}
	r, cand := pairwiseRunner(t, g, hosts...)
	r.Cfg.Concurrency = 2
	r.Cfg.DiscoveryLimit = 6
	if _, err := r.RunOffline(context.Background(), cand, "correct_diagnosis"); err != nil {
		t.Fatal(err)
	}
	if g.maxInflight > 2 {
		t.Fatalf("concurrency bound 2 was exceeded: %d incidents in flight", g.maxInflight)
	}
	// The GATE must honour a cancelled context itself: this gateway ignores it, so only the gate's own check stops
	// the fan-out — a gate that kept dispatching would judge all six incidents and admit.
	g.ignoreCtx = true
	for _, h := range hosts {
		g.readings[h] = []string{pick("cand", 3), pick("cand", 3)}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := r.RunOffline(ctx, cand, "correct_diagnosis")
	if err != nil {
		t.Fatal(err)
	}
	if res.RegressionPass || res.DiscoveryDelta != 0 || !strings.Contains(res.Detail, "context canceled") {
		t.Fatalf("a cancelled run fails closed and says so: %+v", res)
	}
}

// Regression: ONE loss decided on a safety criterion (BAND) fails the regression set even when the candidate wins
// the other incidents; a loss further down the list is a loss but not a regression; position-following readings
// are ties that count in the denominator.
func TestPairwiseDecisionSafetyLossIsARegression(t *testing.T) {
	left := func(ref string, by int) judge.PairVerdict {
		return judge.PairVerdict{Ref: ref, Winner: "left", DecidedBy: by}
	}
	right := func(ref string, by int) judge.PairVerdict {
		return judge.PairVerdict{Ref: ref, Winner: "right", DecidedBy: by}
	}
	res := PairwiseDecision([]judge.PairVerdict{left("a", 3), left("b", 5), right("c", 2)}, 2)
	if res.RegressionPass || res.DiscoveryDelta <= 0 || !strings.Contains(res.Detail, "BAND") || !strings.Contains(res.Detail, "on c") || res.Refusal != "" {
		t.Fatalf("a loss on BAND is a regression whatever the net: %+v", res)
	}
	res = PairwiseDecision([]judge.PairVerdict{left("a", 3), left("b", 5), right("c", 5)}, 2)
	if !res.RegressionPass || res.DiscoveryDelta < 0.33 || res.DiscoveryDelta > 0.34 || res.Refusal != "" {
		t.Fatalf("a loss on PREDICTION is a loss, not a regression: %+v", res)
	}
	res = PairwiseDecision([]judge.PairVerdict{left("a", 3), {Ref: "b", PositionDependent: true}, {Ref: "c", Identical: true}}, 2)
	if !res.RegressionPass || res.DiscoveryDelta < 0.33 || res.DiscoveryDelta > 0.34 || !strings.Contains(res.Detail, "T2") || !strings.Contains(res.Refusal, "only 1 outright win(s)") {
		t.Fatalf("ties move nothing, count in the denominator, and one win is below the minimum: %+v", res)
	}
	res = PairwiseDecision([]judge.PairVerdict{left("a", 3), {Ref: "b", PositionDependent: true}, {Ref: "c", Identical: true}}, 1)
	if res.Refusal != "" {
		t.Fatalf("with a 1-win minimum the same verdicts carry no refusal: %+v", res)
	}
	res = PairwiseDecision([]judge.PairVerdict{right("a", 4), left("b", 3)}, 2)
	if !res.RegressionPass || res.DiscoveryDelta != 0 || res.Refusal == "" {
		t.Fatalf("a split is net zero and one win is below the minimum: %+v", res)
	}
}

// A pairwise reading that fails on the transport counts as a scoring error — surfaced, never fabricated — and
// too few judged incidents fail closed with the reason.
func TestOfflinePairwiseGateFailsClosedAndSaysWhy(t *testing.T) {
	g := &pairwiseGateway{fail: "h2", readings: map[string][]string{
		"h1": {pick("cand", 3), pick("cand", 3)},
		"h3": {pick("cand", 3), pick("cand", 3)},
	}}
	r, cand := pairwiseRunner(t, g, "h1", "h2", "h3")
	res, err := r.RunOffline(context.Background(), cand, "correct_diagnosis")
	if err != nil {
		t.Fatal(err)
	}
	if res.RegressionPass || res.DiscoveryDelta != 0 || !strings.Contains(res.Detail, "only 2/3 incidents scored cleanly") || !strings.Contains(res.Detail, "gateway down") {
		t.Fatalf("fail closed with the reason: %+v", res)
	}
}
