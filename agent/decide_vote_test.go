package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
)

// TG-46 fixtures — forced-decision samples. Every proposal cites tr-1 (the id readTool captures) so a
// winner survives the citation gate after gathered observations; the dv prefix keeps them distinct from
// agent_test.go's shared constants.
const (
	dvProposeRestartHi  = `{"action":"propose","confidence":0.9,"proposal":{"external_ref":"TG-46","target":"web01","op_class":"restart-service","op":"restart","reversible":true,"confidence":0.9,"evidence_ids":["tr-1"]}}`
	dvProposeRestartMid = `{"action":"propose","confidence":0.75,"proposal":{"external_ref":"TG-46","target":"web01","op_class":"restart-service","op":"restart","reversible":true,"confidence":0.75,"evidence_ids":["tr-1"]}}`
	dvProposeResize     = `{"action":"propose","confidence":0.8,"proposal":{"external_ref":"TG-46","target":"db01","op_class":"resize-disk","op":"resize","reversible":true,"confidence":0.8,"evidence_ids":["tr-1"]}}`
	dvProposeResizeLow  = `{"action":"propose","confidence":0.6,"proposal":{"external_ref":"TG-46","target":"db01","op_class":"resize-disk","op":"resize","reversible":true,"confidence":0.6,"evidence_ids":["tr-1"]}}`
	dvProposeIrrev      = `{"action":"propose","confidence":0.9,"proposal":{"external_ref":"TG-46","target":"db02","op_class":"rebuild-array","op":"rebuild","reversible":false,"confidence":0.9,"evidence_ids":["tr-1"]}}`
	dvStop              = `{"action":"stop","confidence":0.8,"reason":"the disk pressure already cleared; no action warranted","evidence_ids":["tr-1"]}`
	dvToolA             = `{"action":"tool","tool":"get-logs","args":{"host":"a1"},"confidence":0.8}`
	dvToolB             = `{"action":"tool","tool":"get-logs","args":{"host":"b1"},"confidence":0.8}`
	dvGarbage           = `the service looks unhealthy, I would restart it`
)

// permute3 returns every arrival order of three samples — the determinism oracle's input space.
func permute3(a, b, c string) [][]string {
	return [][]string{
		{a, b, c}, {a, c, b}, {b, a, c}, {b, c, a}, {c, a, b}, {c, b, a},
	}
}

// A 2-1 majority selects MECHANICALLY: the (propose, restart-service) key outvotes the stop, the winner is
// the highest-confidence member of the winning key, and the dissenting stop is counted — never silently
// dropped. No tie, so the conservative rule stays out of it.
func TestDecideMajoritySelectsMechanically(t *testing.T) {
	winner, samples, disagreement, tie := decideByMajority([]string{dvProposeRestartMid, dvStop, dvProposeRestartHi})
	if winner != dvProposeRestartHi {
		t.Fatalf("the majority key's highest-confidence sample must win, got %q", winner)
	}
	if disagreement != 1 || tie {
		t.Fatalf("2-of-3 agreement is disagreement=1 with no tie, got disagreement=%d tie=%v", disagreement, tie)
	}
	if len(samples) != 3 || samples[0].Kind != "propose" || samples[0].OpClass != "restart-service" ||
		samples[1].Kind != "stop" || samples[2].Kind != "propose" {
		t.Fatalf("every sample's structured decision must be recorded in draw order: %+v", samples)
	}
}

// THE TIE RULE (red-proved by inverting moreConservative's rank comparison): a 3-way split holds no
// majority, and the conservative resolution selects the STOP — a stand-down actuates nothing, so it beats
// every actuation when the samples cannot agree. The tie is reported for the loud marker.
func TestDecideTieStopBeatsActuate(t *testing.T) {
	winner, _, disagreement, tie := decideByMajority([]string{dvProposeRestartHi, dvStop, dvProposeResize})
	if winner != dvStop {
		t.Fatalf("a split vote must resolve to the stand-down (stop beats actuate), got %q", winner)
	}
	if !tie || disagreement != 2 {
		t.Fatalf("a 3-way split is a broken tie with 2 dissenters, got tie=%v disagreement=%d", tie, disagreement)
	}
}

// A tie WITHOUT a stop still resolves down the band-ceiling ladder: the irreversible proposal (whose shape
// the downstream classifier clamps to POLL_PAUSE — a human adjudicates) beats the auto-eligible reversible
// one; among equal ceilings the LOWER confidence wins (the loop escalates low confidence to an operator).
func TestDecideTieAmongProposalsPrefersTheLowestBandCeiling(t *testing.T) {
	winner, _, _, tie := decideByMajority([]string{dvProposeRestartHi, dvProposeIrrev})
	if !tie || winner != dvProposeIrrev {
		t.Fatalf("on a proposal-only tie the POLL_PAUSE-ceiling (irreversible) sample must win, got tie=%v %q", tie, winner)
	}
	winner, _, _, tie = decideByMajority([]string{dvProposeRestartHi, dvProposeResizeLow})
	if !tie || winner != dvProposeResizeLow {
		t.Fatalf("on an equal-ceiling tie the lower-confidence sample must win, got tie=%v %q", tie, winner)
	}
}

// Determinism where it matters: the same three samples produce the same winner and the same counts in
// EVERY arrival order — majority, tie, and no-decision fallback alike.
func TestDecideOrderIndependence(t *testing.T) {
	for name, tri := range map[string][3]string{
		"majority":    {dvProposeRestartMid, dvStop, dvProposeRestartHi},
		"3-way-split": {dvProposeRestartHi, dvStop, dvProposeResize},
		"no-decision": {dvToolA, dvGarbage, dvToolB},
	} {
		wantWinner, _, wantDis, wantTie := decideByMajority([]string{tri[0], tri[1], tri[2]})
		for _, p := range permute3(tri[0], tri[1], tri[2]) {
			winner, _, dis, tie := decideByMajority(p)
			if winner != wantWinner || dis != wantDis || tie != wantTie {
				t.Fatalf("%s: order %v changed the outcome: winner %q vs %q, disagreement %d vs %d, tie %v vs %v",
					name, p, winner, wantWinner, dis, wantDis, tie, wantTie)
			}
		}
	}
}

// When NO sample decided (tool calls / unparseable prose), the vote cannot invent a decision: the
// deterministic fallback re-enters one drawn text into the normal path, and EVERY sample counts as a
// dissenter — the whole draw failed the forced decision and the record says so.
func TestDecideNoSampleDecidedFallsBackDeterministically(t *testing.T) {
	raws := []string{dvToolA, dvGarbage, dvToolB}
	min := raws[0]
	for _, r := range raws[1:] {
		if r < min {
			min = r
		}
	}
	winner, samples, disagreement, tie := decideByMajority(raws)
	if winner != min {
		t.Fatalf("with no decided sample the lexicographically smallest raw must win (order-independent), got %q want %q", winner, min)
	}
	if disagreement != 3 || tie {
		t.Fatalf("a draw with zero decisions is total disagreement, got disagreement=%d tie=%v", disagreement, tie)
	}
	if samples[0].Kind != "tool" || samples[1].Kind != "invalid" || samples[2].Kind != "tool" {
		t.Fatalf("undecided kinds must still be recorded: %+v", samples)
	}
}

// THE WIRING ORACLE. A DecideSampleN=3 session: the five investigation cycles make EXACTLY one call each
// on the investigate tier (the sampling never applies to them), the ONE forced-decision cycle draws
// exactly three samples on the decision tier, the majority's best statement becomes the terminal proposal
// through the unchanged parse path, and the draw record lands on the Result and the cycle transcript.
func TestForcedDecisionCycleDrawsNSamplesAndVotes(t *testing.T) {
	m := &modelRecorder{responses: []string{
		distinctToolCall("h1"), distinctToolCall("h2"), distinctToolCall("h3"), distinctToolCall("h4"), distinctToolCall("h5"),
		dvProposeRestartMid, dvStop, dvProposeRestartHi,
	}}
	ts := NewReadOnlyToolSet()
	_ = ts.Register(readTool{})
	ag := &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "fast", DecisionModelName: "primary", User: "t", DecideSampleN: 3}
	res, err := ag.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.models) != 8 {
		t.Fatalf("5 single-call investigation cycles + 3 decide samples = 8 model calls, got %d (%v)", len(m.models), m.models)
	}
	for i, tier := range m.models[:5] {
		if tier != "fast" {
			t.Fatalf("investigation cycle %d must make its ONE call on the investigate tier, got %q — sampling leaked into investigation", i+1, tier)
		}
	}
	for i, tier := range m.models[5:] {
		if tier != "primary" {
			t.Fatalf("decide sample %d must run on the decision tier, got %q", i+1, tier)
		}
	}
	if res.Outcome != OutcomeProposed || res.Proposal.Action.OpClass != "restart-service" || res.Confidence != 0.9 {
		t.Fatalf("the majority (propose/restart-service) at its highest confidence must be the terminal decision, got %v %q conf=%v",
			res.Outcome, res.Proposal.Action.OpClass, res.Confidence)
	}
	if len(res.DecideSamples) != 3 || res.DecideDisagreement != 1 || res.DecideTieBroken {
		t.Fatalf("the draw record must carry all 3 samples and the 1 dissenter: %+v disagreement=%d tie=%v",
			res.DecideSamples, res.DecideDisagreement, res.DecideTieBroken)
	}
	if res.Cycles != 6 || len(res.Steps) != 6 {
		t.Fatalf("sampling must not inflate the cycle count: cycles=%d steps=%d", res.Cycles, len(res.Steps))
	}
	if got := res.Steps[5].Observation; got != "decide-samples: 3 drawn, 1 dissent" {
		t.Fatalf("the decide cycle's transcript row must name the draw, got %q", got)
	}
}

// A split draw wired end to end: the conservative stand-down is the terminal outcome and the tie flag
// rides the Result for the activity's loud marker.
func TestForcedDecisionSplitResolvesConservativelyWired(t *testing.T) {
	m := &modelRecorder{responses: []string{
		distinctToolCall("h1"), distinctToolCall("h2"), distinctToolCall("h3"), distinctToolCall("h4"), distinctToolCall("h5"),
		dvProposeRestartHi, dvStop, dvProposeResize,
	}}
	ts := NewReadOnlyToolSet()
	_ = ts.Register(readTool{})
	ag := &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "fast", DecisionModelName: "primary", User: "t", DecideSampleN: 3}
	res, err := ag.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeStop || res.Reason != "agent requested stop" {
		t.Fatalf("the split must resolve to the grounded stand-down, got %v %q", res.Outcome, res.Reason)
	}
	if !res.DecideTieBroken || res.DecideDisagreement != 2 {
		t.Fatalf("the split must be recorded loudly: tie=%v disagreement=%d", res.DecideTieBroken, res.DecideDisagreement)
	}
}

// N ≤ 1 IS THE OLD PATH, BYTE FOR BYTE: exactly one decide call, no draw record — for both the zero value
// every non-activity caller leaves and an explicit dial-back to 1.
func TestDecideSampleWidthOneIsTheSingleCallPath(t *testing.T) {
	stopGrounded := `{"action":"stop","confidence":0.9,"reason":"cleared","evidence_ids":["tr-1"]}`
	for _, n := range []int{0, 1} {
		m := &modelRecorder{responses: []string{
			distinctToolCall("h1"), distinctToolCall("h2"), distinctToolCall("h3"), distinctToolCall("h4"), distinctToolCall("h5"),
			stopGrounded,
		}}
		ts := NewReadOnlyToolSet()
		_ = ts.Register(readTool{})
		ag := &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "fast", DecisionModelName: "primary", User: "t", DecideSampleN: n}
		res, err := ag.Run(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(m.models) != 6 {
			t.Fatalf("DecideSampleN=%d must keep the single-call decide (6 calls total), got %d", n, len(m.models))
		}
		if res.DecideSamples != nil || res.DecideDisagreement != 0 || res.DecideTieBroken {
			t.Fatalf("DecideSampleN=%d must record NO draw: %+v", n, res.DecideSamples)
		}
	}
}

// A session that decides on its own — no poll-limit nudge, so no FORCED-decision cycle — never samples,
// whatever the width says: the knob binds to the forced cycle, not to deciding per se.
func TestSelfConvergedDecisionNeverSamples(t *testing.T) {
	m := &modelRecorder{responses: []string{proposeHigh}}
	ts := NewReadOnlyToolSet()
	_ = ts.Register(readTool{})
	ag := &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "fast", DecisionModelName: "primary", User: "t", DecideSampleN: 3}
	res, err := ag.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.models) != 1 || res.DecideSamples != nil {
		t.Fatalf("a self-converged decision is one call with no draw record, got %d calls %+v", len(m.models), res.DecideSamples)
	}
	if res.Outcome != OutcomeProposed {
		t.Fatalf("setup: expected an immediate proposal, got %v", res.Outcome)
	}
}

// errAtCallModel fails the errAt-th Complete call (1-based) and delegates every other call.
type errAtCallModel struct {
	inner *modelRecorder
	errAt int
	calls int
}

func (m *errAtCallModel) Complete(ctx context.Context, user, modelName string, msgs []model.Message) (string, error) {
	m.calls++
	if m.calls == m.errAt {
		return "", errors.New("gateway 502")
	}
	return m.inner.Complete(ctx, user, modelName, msgs)
}

// The width is best-effort, the content is not: a MID-draw model error stops drawing and the vote runs
// over the samples already held — hardening deep/critical decisions must not multiply the session's
// failure surface by the sample count. The record shows the short draw (2 drawn).
func TestDecideMidDrawErrorVotesOverHeldSamples(t *testing.T) {
	inner := &modelRecorder{responses: []string{
		distinctToolCall("h1"), distinctToolCall("h2"), distinctToolCall("h3"), distinctToolCall("h4"), distinctToolCall("h5"),
		dvProposeRestartMid, dvProposeRestartHi,
	}}
	m := &errAtCallModel{inner: inner, errAt: 8} // the third decide draw fails
	ts := NewReadOnlyToolSet()
	_ = ts.Register(readTool{})
	ag := &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "fast", DecisionModelName: "primary", User: "t", DecideSampleN: 3}
	res, err := ag.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeProposed || res.Confidence != 0.9 {
		t.Fatalf("the two held samples must still vote (winner conf 0.9), got %v conf=%v", res.Outcome, res.Confidence)
	}
	if len(res.DecideSamples) != 2 {
		t.Fatalf("the record must show the SHORT draw of 2, got %d", len(res.DecideSamples))
	}
	if got := res.Steps[5].Observation; got != "decide-samples: 2 drawn, 0 dissent" {
		t.Fatalf("the transcript must name the short draw, got %q", got)
	}
}

// A FIRST-draw failure takes exactly the single-call error path — the first sample IS the plain decide
// call, so sampling adds no new failure mode ahead of it.
func TestDecideFirstDrawErrorFailsLikeTheSingleCallPath(t *testing.T) {
	inner := &modelRecorder{responses: []string{
		distinctToolCall("h1"), distinctToolCall("h2"), distinctToolCall("h3"), distinctToolCall("h4"), distinctToolCall("h5"),
	}}
	m := &errAtCallModel{inner: inner, errAt: 6} // the forced-decision cycle's first (plain) call fails
	ts := NewReadOnlyToolSet()
	_ = ts.Register(readTool{})
	ag := &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "fast", DecisionModelName: "primary", User: "t", DecideSampleN: 3}
	res, err := ag.Run(context.Background(), nil)
	if err == nil {
		t.Fatal("a first-draw model failure must surface as the single-call error path")
	}
	if res.Outcome != OutcomeStop || res.Reason != "model call failed" {
		t.Fatalf("expected the unchanged model-call-failed stop, got %v %q", res.Outcome, res.Reason)
	}
	if res.DecideSamples != nil || m.calls != 6 {
		t.Fatalf("no draw may be recorded and no extra call made after the failure: samples=%+v calls=%d", res.DecideSamples, m.calls)
	}
}
