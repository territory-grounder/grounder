package skillgen

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/skillstore"
	"github.com/territory-grounder/grounder/eval"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// fakeRecordings serves a recording for the refs it knows and an EMPTY recording for the rest.
type fakeRecordings struct{ have map[string]bool }

func (f fakeRecordings) Load(_ context.Context, ref string) (eval.RecordedSession, error) {
	rec := eval.RecordedSession{ExternalRef: ref}
	if f.have[ref] {
		rec.Calls = []eval.RecordedCall{{Cycle: 1, Tool: "get-device-status", EvidenceID: "lnms-dev-" + ref, Output: "recorded", Success: true}}
	}
	return rec, nil
}

// fakeProbe stands in for runner.ReplayProbe: it records which incident and which BODY it was asked to run (by
// applying the override to a sample row) and answers a scripted deliverable per body marker — the candidate body
// proposes, the production body stops — so the content-preferring pairwise fake picks the candidate.
type fakeProbe struct {
	mu     sync.Mutex
	calls  []string // "<ref>|<body-marker>|k"
	fail   string   // ref whose probe fails
	split  bool     // candidate replicates disagree with each other (k=2)
	tools  int      // tool-set factories invoked
	seenFn func() *agent.ToolSet
}

func (f *fakeProbe) run(_ context.Context, _ runner.ProbeDeps, newTools func() *agent.ToolSet, inc runner.ProbeIncident, override func([]skillstore.ProductionRow) []skillstore.ProductionRow, k int) ([]runner.ProbeDeliverable, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := override([]skillstore.ProductionRow{{SkillName: "triage-protocol", Body: "ORIGINAL"}})
	body := rows[0].Body
	f.calls = append(f.calls, inc.ExternalRef+"|"+body+"|"+itoa(k))
	if newTools() != nil {
		f.tools++
	}
	if inc.ExternalRef == f.fail {
		return nil, errors.New("probe loop blew up")
	}
	var out []runner.ProbeDeliverable
	for i := 0; i < k; i++ {
		if strings.Contains(body, "CANDIDATE") {
			d := runner.ProbeDeliverable{Band: "AUTO_NOTICE", Outcome: "proposed", Proposed: true, Op: "rotate-logs", Prediction: "usage falls", Evidence: []string{"lnms-dev-" + inc.ExternalRef}}
			if f.split && i == 1 {
				d = runner.ProbeDeliverable{Outcome: "stop", Conclusion: "unsure"} // the second candidate replicate stops
			}
			out = append(out, d)
			continue
		}
		out = append(out, runner.ProbeDeliverable{Outcome: "stop", Conclusion: "escalate"})
	}
	return out, nil
}

func itoa(n int) string { return string(rune('0' + n)) }

// With the probe armed, an incident WITH a recording is judged on real-loop deliverables for both bodies (the
// candidate body swapped in), its divergence note is stored and marked probed; an incident WITHOUT a recording goes
// single-shot (triage calls) and is marked not probed; the detail states the split.
func TestOfflineGateProbesRecordedIncidentsAndFallsBackOtherwise(t *testing.T) {
	g := &pairwiseGateway{readings: map[string][]string{
		"h1": {pick("cand", 3), pick("cand", 3)},
		"h2": {pick("cand", 3), pick("cand", 3)},
		"h3": {pick("cand", 3), pick("cand", 3)},
	}}
	r, cand := pairwiseRunner(t, g, "h1", "h2", "h3")
	fp := &fakeProbe{}
	r.Probe = &ProbeConfig{Replicates: 1, Recordings: fakeRecordings{have: map[string]bool{"inc-h1": true, "inc-h2": true}}, Run: fp.run}
	res, err := r.RunOffline(context.Background(), cand, "correct_diagnosis")
	if err != nil {
		t.Fatal(err)
	}
	if !res.RegressionPass || res.DiscoveryDelta != 1 {
		t.Fatalf("the candidate's real-loop deliverables propose where production stops: %+v", res)
	}
	calls := strings.Join(fp.calls, "\n")
	for _, want := range []string{"inc-h1|CANDIDATE-BODY|1", "inc-h1|PRODUCTION-BODY|1", "inc-h2|CANDIDATE-BODY|1", "inc-h2|PRODUCTION-BODY|1"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("the probe must run both bodies for every recorded incident, missing %q in:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "inc-h3|") {
		t.Fatal("an incident without a recording must not be probed")
	}
	if n := len(g.tiers["flywheel-offline-triage"]); n != 2 {
		t.Fatalf("only the unrecorded incident goes single-shot (2 triage calls), got %d", n)
	}
	if len(res.Divergence) != 2 || !res.Divergence[0].Probed || res.Divergence[0].Ref != "inc-h1" || !strings.Contains(res.Divergence[0].Candidate, "agreed") {
		t.Fatalf("divergence notes for the probed incidents only: %+v", res.Divergence)
	}
	if !strings.Contains(res.Detail, "2 probed through the real loop") || !strings.Contains(res.Detail, "1 single-shot") {
		t.Fatalf("the detail must state the probed/single-shot split: %s", res.Detail)
	}
}

// k=2: a candidate whose replicates disagree is reported as DIVERGED, and aligned pairs that disagree on the winner
// are a tie that says replicate-dependent — never a win built on one lucky replicate.
func TestOfflineGateProbeDivergenceAndReplicateDependentTie(t *testing.T) {
	g := &pairwiseGateway{readings: map[string][]string{
		"h1": {pick("cand", 3), pick("cand", 3), pick("cand", 3), pick("cand", 3)}, // two aligned pairs, two readings each
		"h2": {pick("cand", 3), pick("cand", 3), pick("cand", 3), pick("cand", 3)},
		"h3": {pick("cand", 3), pick("cand", 3), pick("cand", 3), pick("cand", 3)},
	}}
	r, cand := pairwiseRunner(t, g, "h1", "h2", "h3")
	fp := &fakeProbe{split: true}
	r.Probe = &ProbeConfig{Replicates: 2, Recordings: fakeRecordings{have: map[string]bool{"inc-h1": true, "inc-h2": true, "inc-h3": true}}, Run: fp.run}
	res, err := r.RunOffline(context.Background(), cand, "correct_diagnosis")
	if err != nil {
		t.Fatal(err)
	}
	// Pair 1: candidate proposes vs production stops → candidate. Pair 2: candidate STOPS vs production stops →
	// identical deliverables → tie. The two aligned pairs disagree → replicate-dependent tie on every incident.
	if res.DiscoveryDelta != 0 || res.Refusal == "" || !strings.Contains(res.Detail, "W0/L0/T3") {
		t.Fatalf("replicate-dependent preferences must be ties: %+v", res)
	}
	if len(res.Divergence) != 3 || !strings.Contains(res.Divergence[0].Candidate, "DIVERGED") || !strings.Contains(res.Divergence[0].Production, "agreed") {
		t.Fatalf("the candidate's replicates diverged, production's agreed: %+v", res.Divergence)
	}
	for _, v := range res.Pairwise {
		if v.Winner != "" || !strings.Contains(strings.Join(v.Why, " "), "replicate-dependent") {
			t.Fatalf("the verdict must say the preference depended on the replicate: %+v", v)
		}
	}
}

// The sessions the proposer READ (cited in the candidate's rationale as `<ref>:[STEP n]`) are excluded from the
// discovery set and named; a probe failure on one incident is a scoring error, never a fabricated verdict.
func TestOfflineGateExcludesCitedSessionsAndSurfacesProbeErrors(t *testing.T) {
	g := &pairwiseGateway{readings: map[string][]string{
		"h2": {pick("cand", 3), pick("cand", 3)},
		"h3": {pick("cand", 3), pick("cand", 3)},
		"h4": {pick("cand", 3), pick("cand", 3)},
	}}
	r, cand := pairwiseRunner(t, g, "h1", "h2", "h3", "h4")
	cand.Rationale = "[draft] eval failure — proposer INV; edits cite the record:\nrevised -> stop earlier -> inc-h1:[STEP 2] the agent re-read the same host\nadded -> cite the disk id -> inc-h1:[STEP 3] evidence not cited"
	fp := &fakeProbe{}
	r.Probe = &ProbeConfig{Replicates: 1, Recordings: fakeRecordings{have: map[string]bool{"inc-h1": true, "inc-h2": true, "inc-h3": true, "inc-h4": true}}, Run: fp.run}
	res, err := r.RunOffline(context.Background(), cand, "correct_diagnosis")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(fp.calls, "\n"), "inc-h1|") {
		t.Fatal("the session the proposer read must not be judged on")
	}
	if !strings.Contains(res.Detail, "excluded 1 incident(s) the proposer read (inc-h1)") || !res.RegressionPass || res.DiscoveryDelta != 1 {
		t.Fatalf("exclusion must be named and the rest judged: %+v", res)
	}
	// A probe that fails on one incident: scoring error, surfaced; the others still judged.
	fp2 := &fakeProbe{fail: "inc-h2"}
	r.Probe = &ProbeConfig{Replicates: 1, Recordings: fakeRecordings{have: map[string]bool{"inc-h2": true, "inc-h3": true, "inc-h4": true}}, Run: fp2.run}
	g.readings = map[string][]string{"h3": {pick("cand", 3), pick("cand", 3)}, "h4": {pick("cand", 3), pick("cand", 3)}}
	res2, err := r.RunOffline(context.Background(), cand, "correct_diagnosis")
	if err != nil {
		t.Fatal(err)
	}
	if res2.RegressionPass || !strings.Contains(res2.Detail, "only 2/3 incidents scored cleanly") || !strings.Contains(res2.Detail, "probe loop blew up") {
		t.Fatalf("a failed probe is a surfaced scoring error (and 2 < MinIncidents 3 fails closed): %+v", res2)
	}
}

// CitedRefs reads the refs out of TG-582 edit rows (ref, or ref/rollout for multi-rollout packs) and nothing else.
func TestCitedRefsReadsTheProposersRows(t *testing.T) {
	refs := skillstore.CitedRefs("[draft] x — proposer DOM; edits cite the record:\nkept -> a -> inc-9:[STEP 1] m\nrevised -> b -> inc-10/live:[STEP 4] m\nnot a row: inc-11:[STEP 2]\n")
	if len(refs) != 2 || !refs["inc-9"] || !refs["inc-10"] {
		t.Fatalf("want inc-9 and inc-10 only, got %v", refs)
	}
	if len(skillstore.CitedRefs("plain rationale, no rows")) != 0 {
		t.Fatal("a rationale without rows cites nothing")
	}
}

// The aligned fold never hides a safety loss: a candidate that lost on BAND in ONE of k replicate pairs is the
// incident's loss on BAND (a regression), whatever the other pairs did; pairs that all followed the position stay a
// positional tie; a genuine direction disagreement is a replicate-dependent tie.
func TestAggregateAlignedPropagatesASafetyLoss(t *testing.T) {
	win := judge.PairVerdict{Ref: "x", Winner: "left", DecidedBy: 3, Calls: 2}
	lossBand := judge.PairVerdict{Ref: "x", Winner: "right", DecidedBy: 2, Calls: 2}
	lossLate := judge.PairVerdict{Ref: "x", Winner: "right", DecidedBy: 5, Calls: 2}
	pos := judge.PairVerdict{Ref: "x", PositionDependent: true, Calls: 2}
	if v := aggregateAligned([]judge.PairVerdict{win, lossBand}); v.Winner != "right" || v.DecidedBy != 2 || !strings.Contains(strings.Join(v.Why, " "), "safety") {
		t.Fatalf("a BAND loss in one pair must be the verdict: %+v", v)
	}
	if v := aggregateAligned([]judge.PairVerdict{win, lossLate}); v.Winner != "" || !v.ReplicateDependent {
		t.Fatalf("a direction disagreement off the safety criteria is a replicate-dependent tie: %+v", v)
	}
	if v := aggregateAligned([]judge.PairVerdict{pos, pos}); !v.PositionDependent || v.ReplicateDependent || v.Winner != "" {
		t.Fatalf("all-positional pairs stay a positional tie: %+v", v)
	}
	if v := aggregateAligned([]judge.PairVerdict{win, win}); v.Winner != "left" || v.DecidedBy != 3 || v.Calls != 4 {
		t.Fatalf("agreement folds to the winner: %+v", v)
	}
	res := PairwiseDecision([]judge.PairVerdict{aggregateAligned([]judge.PairVerdict{win, lossBand}), win, win}, 2)
	if res.RegressionPass {
		t.Fatalf("the folded safety loss must fail the regression set: %+v", res)
	}
}

// realPathModel drives the REAL loop inside the real ReplayProbe over a real replay tool-set: it asks for the
// recorded device status until the recorded observation is in the conversation, then proposes under the candidate
// body and stops under the production body.
type realPathModel struct {
	mu   sync.Mutex
	seen []string
}

func (m *realPathModel) Complete(_ context.Context, _, _ string, msgs []model.Message) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := ""
	for _, mm := range msgs {
		m.seen = append(m.seen, mm.Content)
		all += mm.Content + "\n"
	}
	if !strings.Contains(all, "RECORDED-OBSERVATION-MARKER") {
		return `{"action":"tool","tool":"get-device-status","args":{"host":"h1"},"confidence":0.8}`, nil
	}
	if strings.Contains(all, "CANDIDATE-BODY") {
		return `{"action":"propose","confidence":0.85,"evidence_ids":["lnms-dev-h1"],"proposal":{"external_ref":"inc-h1","target":"h1","op_class":"restart-service","op":"restart","reversible":true,"evidence_ids":["lnms-dev-h1"],"prediction":"the device answers within 2 minutes"}}`, nil
	}
	return `{"action":"stop","confidence":0.9,"conclusion":"escalate to a human"}`, nil
}

// The DEFAULT probe path — no fake Run — reaches runner.ReplayProbe over eval.NewReplayToolSet: the agent's real
// tool call is answered from the RECORDING (the marker reaches the model), the candidate body is what the agent
// reads, and the gate's verdict comes from those real-loop deliverables.
func TestOfflineGateDefaultProbePathRunsTheRealLoopOverTheRecording(t *testing.T) {
	g := &pairwiseGateway{readings: map[string][]string{"h1": {pick("cand", 3), pick("cand", 3)}, "h2": {pick("cand", 3), pick("cand", 3)}, "h3": {pick("cand", 3), pick("cand", 3)}}}
	r, cand := pairwiseRunner(t, g, "h1", "h2", "h3")
	rows := func(context.Context) ([]skillstore.ProductionRow, error) {
		body := "PRODUCTION-BODY: escalate when unsure."
		return []skillstore.ProductionRow{{VersionID: 1, SkillName: "triage-protocol", Version: "1.0.0", Body: body, ContentHash: skillstore.ContentHash(body, skillstore.AppliesWhen{})}}, nil
	}
	loopModel := &realPathModel{}
	// Only h1 has a recording; the recorded observation for host h1 carries the marker the loop model keys on.
	recs := markedRecordings{have: map[string]bool{"inc-h1": true}}
	r.Probe = &ProbeConfig{Replicates: 1, Recordings: recs, Deps: runner.ProbeDeps{Model: loopModel, SkillRows: rows}} // Run nil ⇒ runner.ReplayProbe
	res, err := r.RunOffline(context.Background(), cand, "correct_diagnosis")
	if err != nil {
		t.Fatal(err)
	}
	seen := strings.Join(loopModel.seen, "\n")
	if !strings.Contains(seen, "RECORDED-OBSERVATION-MARKER") {
		t.Fatal("the real loop must receive the RECORDED observation from the replay tool-set")
	}
	if !strings.Contains(seen, "CANDIDATE-BODY") || !strings.Contains(seen, "PRODUCTION-BODY") {
		t.Fatal("both bodies must have been read by the real loop (one probe per body)")
	}
	if len(res.Divergence) != 1 || !res.Divergence[0].Probed || res.Divergence[0].Ref != "inc-h1" {
		t.Fatalf("exactly the recorded incident is probed: %+v", res.Divergence)
	}
	if !res.RegressionPass || res.DiscoveryDelta != 1 {
		t.Fatalf("the candidate's real-loop proposal beats production's stop on every incident: %+v", res)
	}
}

// markedRecordings serves one recorded get-device-status observation for the host, carrying a marker.
type markedRecordings struct{ have map[string]bool }

func (f markedRecordings) Load(_ context.Context, ref string) (eval.RecordedSession, error) {
	rec := eval.RecordedSession{ExternalRef: ref}
	if f.have[ref] {
		host := strings.TrimPrefix(ref, "inc-")
		rec.Calls = []eval.RecordedCall{{Cycle: 1, Tool: "get-device-status", EvidenceID: "lnms-dev-" + host, Output: "RECORDED-OBSERVATION-MARKER: device " + host + " is down since 09:00", Success: true}}
	}
	return rec, nil
}

// ProbeDraft (the composer's Prober): one line per recorded discovery incident — the draft's and production's
// deliverables, their divergence and the pairwise verdict — skipping the sessions the proposer read and incidents
// without a recording; "" when the probe is off.
func TestProbeDraftRendersARecordedIncidentReportForTheComposer(t *testing.T) {
	g := &pairwiseGateway{readings: map[string][]string{"h1": {pick("cand", 3), pick("cand", 3)}, "h3": {pick("cand", 3), pick("cand", 3)}}}
	r, _ := pairwiseRunner(t, g, "h1", "h2", "h3")
	fp := &fakeProbe{}
	r.Probe = &ProbeConfig{Replicates: 1, Recordings: fakeRecordings{have: map[string]bool{"inc-h1": true, "inc-h2": true, "inc-h3": true}}, Run: fp.run}
	report, err := r.ProbeDraft(context.Background(), "triage-protocol", "CANDIDATE-BODY draft", map[string]bool{"inc-h2": true})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(report, "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "inc-h1: candidate → band=\"AUTO_NOTICE\" outcome=\"proposed\" proposed=true op=\"rotate-logs\"") || !strings.Contains(lines[0], "production → band=\"\" outcome=\"stop\"") || !strings.Contains(lines[0], "pairwise: candidate preferred (criterion 3)") || !strings.HasPrefix(lines[1], "inc-h3:") {
		t.Fatalf("report shape:\n%s", report)
	}
	if strings.Contains(strings.Join(fp.calls, "\n"), "inc-h2|") {
		t.Fatal("the excluded session must not be probed for the composer either")
	}
	r.Probe = nil
	if rep, err := r.ProbeDraft(context.Background(), "triage-protocol", "x", nil); err != nil || rep != "" {
		t.Fatalf("probe off ⇒ no report: %q %v", rep, err)
	}
}
