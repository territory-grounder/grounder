package runner

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// keyedModel answers from what the loop SHOWS it, not from a call count: a tool call until an observation is in
// the conversation, then the scripted final decision — so decide-sampling or re-asks cannot desynchronise the
// script, and every message the loop sent is kept for the body oracle.
type keyedModel struct {
	mu    sync.Mutex
	final string
	seen  []string
}

func (m *keyedModel) Complete(_ context.Context, _, _ string, msgs []model.Message) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	observed := false
	for _, mm := range msgs {
		m.seen = append(m.seen, mm.Content)
		if strings.Contains(mm.Content, "web01 nginx down") {
			observed = true
		}
	}
	if !observed {
		return probeToolCall, nil
	}
	return m.final, nil
}

func (m *keyedModel) sawAll() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.seen, "\n")
}

const probeToolCall = `{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`

// probePropose is a grounded, reversible restart of web01 that cites the observation it gathered (INV-11: a
// proposal after an observation must cite evidence_ids or the loop rejects it as ungrounded).
const probePropose = `{"action":"propose","confidence":0.85,"evidence_ids":["tr-1"],"proposal":{"external_ref":"probe-1","target":"web01","op_class":"restart-service","op":"restart","reversible":true,"evidence_ids":["tr-1"],"prediction":"nginx on web01 answers within 2 minutes of the restart"}}`

func probeRows(_ context.Context) ([]skillstore.ProductionRow, error) {
	body := "PRODUCTION-BODY-MARKER: read the logs before proposing."
	return []skillstore.ProductionRow{{VersionID: 777, SkillName: "triage-protocol", Version: "9.9.9", Body: body, ContentHash: skillstore.ContentHash(body, skillstore.AppliesWhen{})}}, nil
}

func probeTools() *agent.ToolSet {
	tools := agent.NewReadOnlyToolSet()
	_ = tools.Register(readTool{})
	return tools
}

// countingTools is a tool-set factory that counts how many fresh sets the probe asked for.
type countingTools struct{ n int }

func (c *countingTools) new() *agent.ToolSet { c.n++; return probeTools() }

// The probe runs the REAL loop outside Temporal (no heartbeat panic even with the durable checkpoint armed), k
// times, with the candidate body swapped into the rows the composer reads; a proposing replicate carries the
// production classify stage's band; the deliverables' divergence is counted.
func TestReplayProbeRunsTheRealLoopWithTheCandidateBody(t *testing.T) {
	t.Setenv("TG_INVESTIGATE_DURABLE_CHECKPOINT", "1") // the heartbeat path — must be a no-op outside an activity
	inc := ProbeIncident{ExternalRef: "probe-1", SourceID: "test", AlertRule: "ServiceDown", Host: "web01", Severity: ingest.SeverityCritical, Site: "nl"}
	proposer := &keyedModel{final: probePropose}
	factory := &countingTools{}
	reps, err := ReplayProbe(context.Background(), ProbeDeps{Model: proposer, SkillRows: probeRows}, factory.new, inc, WithBody("triage-protocol", "PROBE-CANDIDATE-BODY: restart first, ask later."), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(reps) != 2 {
		t.Fatalf("want 2 replicates, got %d", len(reps))
	}
	if factory.n != 2 {
		t.Fatalf("every replicate must get a FRESH tool-set (the replay cursor is per set), got %d for 2 replicates", factory.n)
	}
	seen := proposer.sawAll()
	if !strings.Contains(seen, "PROBE-CANDIDATE-BODY") || strings.Contains(seen, "PRODUCTION-BODY-MARKER") {
		t.Fatalf("the agent must read the CANDIDATE body, not the production one")
	}
	for i, r := range reps {
		if !r.Proposed || r.Op != "restart" || r.Band == "" || r.Steps < 1 || len(r.Tools) == 0 || r.Tools[0] != "get-logs" {
			t.Fatalf("replicate %d must be a classified proposal after a real tool step: %+v", i+1, r)
		}
		if !strings.Contains(strings.Join(r.SkillLoads, " "), "triage-protocol@9.9.9#777:store") {
			t.Fatalf("the swapped row must be the one the composer loaded (hash recomputed): %v", r.SkillLoads)
		}
	}
	if d := Diverge(reps); d.Divergent {
		t.Fatalf("two identical replicates must agree: %+v", d)
	}
	// A stopping body: no band, as live; against the proposing replicates it is divergence.
	stopper := &keyedModel{final: `{"action":"stop","confidence":0.9,"conclusion":"nothing to do here"}`}
	stops, err := ReplayProbe(context.Background(), ProbeDeps{Model: stopper, SkillRows: probeRows}, probeTools, inc, WithBody("triage-protocol", "PROBE-STOP-BODY"), 1)
	if err != nil || len(stops) != 1 || stops[0].Proposed || stops[0].Band != "" {
		t.Fatalf("a stop has no band, as live: %+v err=%v", stops, err)
	}
	if d := Diverge(append(reps, stops...)); !d.Divergent || d.Replicates != 3 || d.Outcomes["proposed"] != 2 {
		t.Fatalf("a proposing and a stopping replicate must read as divergent: %+v", d)
	}
	if s := reps[0].Session(inc); s.Band != reps[0].Band || !s.Proposed || s.Op != "restart" || s.Host != "web01" || s.Ref != "probe-1" || s.Severity != "critical" {
		t.Fatalf("Session must carry the deliverable's facts: %+v", s)
	}
	if _, err := ReplayProbe(context.Background(), ProbeDeps{SkillRows: probeRows}, probeTools, inc, nil, 1); err == nil {
		t.Fatal("a probe without a model must refuse")
	}
}

// Agreement is not divergence; the summary line is deterministic.
func TestDivergeAgreement(t *testing.T) {
	reps := []ProbeDeliverable{{Band: "POLL_PAUSE", Outcome: "proposed", Op: "restart"}, {Band: "POLL_PAUSE", Outcome: "proposed", Op: "restart"}}
	d := Diverge(reps)
	if d.Divergent || !strings.Contains(d.String(), "2 replicate(s) agreed") {
		t.Fatalf("agreement must not read as divergence: %+v %s", d, d.String())
	}
	d2 := Diverge([]ProbeDeliverable{{Band: "AUTO", Outcome: "proposed", Op: "restart"}, {Band: "POLL_PAUSE", Outcome: "proposed", Op: "restart"}})
	if !d2.Divergent || !strings.Contains(d2.String(), `"AUTO"×1 "POLL_PAUSE"×1`) {
		t.Fatalf("band disagreement must be named deterministically: %s", d2.String())
	}
}

// The durable checkpoint rides Temporal heartbeats ONLY when the operator armed it, the ctx is an activity ctx AND
// the Deps do not switch it off. The probe switches it off because it runs inside the GENERATOR's activity ctx —
// with IsActivity alone, three concurrent probe loops would heartbeat on the generator and, on its retry, resume
// another incident's checkpoint as their own conversation (review of slice B).
func TestDurableCheckpointIsOffForProbesEvenInsideAnActivity(t *testing.T) {
	t.Setenv("TG_INVESTIGATE_DURABLE_CHECKPOINT", "1")
	if durableCheckpointArmed(context.Background(), Deps{}) {
		t.Fatal("a plain ctx is never an activity ctx — the heartbeat APIs would panic")
	}
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	probe := func(ctx context.Context) (string, error) {
		return fmt.Sprintf("live=%v probe=%v", durableCheckpointArmed(ctx, Deps{}), durableCheckpointArmed(ctx, Deps{DurableCheckpointOff: true})), nil
	}
	env.RegisterActivity(probe)
	val, err := env.ExecuteActivity(probe)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := val.Get(&got); err != nil {
		t.Fatal(err)
	}
	if got != "live=true probe=false" {
		t.Fatalf("inside an activity: the live Deps ride the heartbeat, the probe's Deps never do; got %s", got)
	}
	t.Setenv("TG_INVESTIGATE_DURABLE_CHECKPOINT", "")
	val, _ = env.ExecuteActivity(probe)
	_ = val.Get(&got)
	if got != "live=false probe=false" {
		t.Fatalf("unarmed: nobody rides the heartbeat; got %s", got)
	}
}
