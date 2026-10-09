package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/correlate"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/ingest"
)

// causalTopoFake is a hand-built estate oracle for the causal-election guard test.
type causalTopoFake struct {
	ind map[string]int
	par map[string]string
}

func (f causalTopoFake) InDegree(host string) int        { return f.ind[host] }
func (f causalTopoFake) RunsOnParent(host string) string { return f.par[host] }

// These pin the COLLAPSE GATE (TG-376) — the workflow decision the cluster identity + election exist to
// drive. The election/identity themselves are proven pure in core/correlate and over a real DB in core/db;
// here the only question is: given the correlation stage's verdict, does the workflow open a session or not?
//
// "Exactly ONE session opens for a 40-member storm" decomposes into two facts these two tests pin:
//   - a correlated member that LOST the election (Elected=false) opens NO investigation session; and
//   - the elected subject (Elected=true) DOES investigate, exactly as before.
// The core/db killing test supplies the third fact — that exactly ONE of the 40 members is the elected one.

// A non-elected member collapses: it attaches as evidence and NEVER runs the agent loop. This is the whole
// fix for 1.000 alerts/session.
//
// KILLING MUTATION (executed, RED): delete the `!cor.Elected` collapse branch in workflow.go (or negate it to
// `cor.Elected`). The member then falls through to InvestigateActivity, investigateCalls becomes 1, and the
// outcome is a real triage instead of collapsed:cluster-member — RED on both assertions.
func TestClusterCollapse_NonElectedMemberOpensNoSession(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(`unused — correlate is mocked to a collapsed verdict, investigate must never run`)
	a := NewActivities(deps)
	registerAll(env, a)

	// The correlation stage says: correlated cascade, durable cluster 4242, elected subject is SOMEONE ELSE.
	env.OnActivity(a.CorrelateActivity, mock.Anything, mock.Anything).Return(CorrelateResult{
		ExecClass:  string(execclass.DeepInvestigation),
		Correlated: true,
		Elected:    false, // this member LOST the election
		ClusterID:  4242,
		ElectedRef: "TG-parent-subject",
		ElectRule:  correlate.ElectRuleIndegree,
		HostNames:  []string{"dc1pve03", "web01"},
		MemberRefs: []string{"TG-parent-subject", "TG-member-collapsed"},
	}, nil)

	// If the gate is broken and the workflow opens a session anyway, this fires — the assertion is it does NOT.
	investigateCalls := 0
	env.OnActivity(a.InvestigateActivity, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Run(func(mock.Arguments) {
		investigateCalls++
	}).Return(InvestigateResult{}, nil)

	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-member-collapsed", SourceID: "prometheus-dc1", AlertRule: "NginxDown",
		Host: "web01", Severity: ingest.SeverityWarning, Site: "dc1",
	})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	_ = env.GetWorkflowResult(&res)

	if investigateCalls != 0 {
		t.Fatalf("a non-elected cluster member opened %d investigation sessions, want 0 — the storm did NOT "+
			"collapse (this is the 1.000 alerts/session defect)", investigateCalls)
	}
	if res.Outcome != "collapsed:cluster-member" {
		t.Fatalf("collapsed member outcome = %q, want %q", res.Outcome, "collapsed:cluster-member")
	}
	if res.Proposed {
		t.Fatalf("a collapsed member must not propose (it opened no session): %+v", res)
	}
	// The record threads the elected subject + cluster id (the NAMES CorrelateResult now returns) so the
	// attachment is auditable, not a silent drop.
	for _, want := range []string{"TG-parent-subject", "4242"} {
		if !strings.Contains(res.Conclusion, want) {
			t.Fatalf("collapse record %q does not name %q — the attach-as-evidence trail is incomplete", res.Conclusion, want)
		}
	}
}

// The ELECTED subject investigates exactly as before — the collapse must not swallow the one session that
// is supposed to open. Guards the other direction: a gate that collapsed EVERYONE would open zero sessions.
func TestClusterCollapse_ElectedSubjectStillInvestigates(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(`unused — investigate is mocked`)
	a := NewActivities(deps)
	registerAll(env, a)

	// Correlated cascade, and THIS member is the elected causal subject.
	env.OnActivity(a.CorrelateActivity, mock.Anything, mock.Anything).Return(CorrelateResult{
		ExecClass:  string(execclass.DeepInvestigation),
		Correlated: true,
		Elected:    true, // this member WON the election — it investigates on behalf of the cluster
		ClusterID:  4242,
		ElectedRef: "TG-parent-subject",
		ElectRule:  correlate.ElectRuleIndegree,
	}, nil)

	investigateCalls := 0
	env.OnActivity(a.InvestigateActivity, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Run(func(mock.Arguments) {
		investigateCalls++
	}).Return(InvestigateResult{Proposed: false, Outcome: agent.OutcomeStop.String(), Reason: "nothing to do"}, nil)

	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-parent-subject", SourceID: "prometheus-dc1", AlertRule: "PVENodeDown",
		Host: "dc1pve03", Severity: ingest.SeverityCritical, Site: "dc1",
	})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	_ = env.GetWorkflowResult(&res)

	if investigateCalls != 1 {
		t.Fatalf("the elected subject ran %d investigation sessions, want 1 — the collapse must not swallow the "+
			"one session that is supposed to open", investigateCalls)
	}
	if res.Outcome == "collapsed:cluster-member" {
		t.Fatalf("the elected subject was collapsed instead of investigating: %+v", res)
	}
}

// THE SILENCING SAFEGUARD (safety-review fix): a correlated cluster collapses a member to evidence ONLY when
// the election was decided by CAUSAL evidence (in-degree / runs_on parent-fanout). A cluster elected only by
// earliest-ref — the estate graph unseeded, so the "cluster" is a TIME COINCIDENCE of three hosts that
// happened to alert together (the shape of a TG-169 false positive) — must NOT collapse, or it would silence
// two genuine incidents. This drives the REAL CorrelateActivity (not a mocked result) so it exercises the
// guard where res.Elected is decided from the election rule.
//
// KILLING MUTATION (executed, RED): drop the `&& correlate.IsCausalRule(el.Rule)` clause from the res.Elected
// guard in correlate.go. The earliest-ref (non-causal) case then sets Elected=false, and the assertion that a
// time-coincidence cluster keeps investigating every member goes RED — the silencing regression is caught.
func TestCorrelateActivity_CollapsesOnlyOnCausalElection(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	// A 3-host correlated window: the parent (earliest) plus two guests; the SUBJECT is the last guest, so it
	// is never the elected subject in either case — without a guard it would collapse in BOTH.
	window := correlate.Window{Span: 10 * time.Minute, Observations: []correlate.Observation{
		{ExternalRef: "zz-parent", Host: "dc1pve03", SourceType: "librenms", AlertRule: "pve-node-down", At: base},
		{ExternalRef: "guest-a", Host: "dc1vmA", SourceType: "librenms", AlertRule: "guest-down", At: base.Add(time.Minute)},
		{ExternalRef: "guest-b", Host: "dc1vmB", SourceType: "librenms", AlertRule: "guest-down", At: base.Add(2 * time.Minute)},
	}}
	subject := CorrelateInput{ExternalRef: "guest-b", Host: "dc1vmB", AlertRule: "guest-down", Severity: "warning", At: base.Add(2 * time.Minute)}

	run := func(topo correlate.Topology) CorrelateResult {
		deps := Deps{
			CorrelationWindow: func(context.Context, time.Time) (correlate.Window, error) { return window, nil },
			ClusterJoin: func(context.Context, int64, string, time.Time, time.Duration) (int64, error) {
				return 999, nil // a durable identity exists — the collapse's other precondition
			},
			ClusterTopology: topo,
		}
		res, err := NewActivities(deps).CorrelateActivity(context.Background(), subject)
		if err != nil {
			t.Fatalf("CorrelateActivity: %v", err)
		}
		if !res.Correlated {
			t.Fatalf("the 3-host window must assess correlated, got %+v", res)
		}
		if res.ElectedRef != "zz-parent" {
			t.Fatalf("both cases must elect the earliest/parent subject zz-parent, got %q", res.ElectedRef)
		}
		return res
	}

	// CAUSAL: in-degree anchors the election on the parent ⇒ the non-subject member collapses.
	causal := run(causalTopoFake{ind: map[string]int{"dc1pve03": 2}})
	if causal.ElectRule != correlate.ElectRuleIndegree {
		t.Fatalf("causal case elect_rule = %q, want %q", causal.ElectRule, correlate.ElectRuleIndegree)
	}
	if causal.Elected {
		t.Fatalf("a causally-elected non-subject member must collapse (Elected=false), got Elected=true: %+v", causal)
	}

	// NON-CAUSAL: no topology ⇒ the election falls back to earliest-ref (a time coincidence) ⇒ NO collapse:
	// every member must still investigate. This is the silencing safeguard.
	coincidence := run(nil)
	if coincidence.ElectRule != correlate.ElectRuleEarliest {
		t.Fatalf("no-topology case elect_rule = %q, want %q (the non-causal fallback)", coincidence.ElectRule, correlate.ElectRuleEarliest)
	}
	if !coincidence.Elected {
		t.Fatalf("a time-coincidence cluster (elected by earliest-ref, no causal anchor) must NOT collapse — "+
			"every member investigates, or two genuine incidents are SILENCED. Got Elected=false: %+v", coincidence)
	}
	// The audit is unchanged either way: the elect_rule + runner-up are still recorded even when the collapse
	// is (correctly) withheld.
	if coincidence.RunnerUpRef == "" {
		t.Fatalf("the non-causal election still records its runner-up for review, got empty: %+v", coincidence)
	}
}

// SAFETY-CONTINGENCY PIN (TG-465 review, finding 1). The straddle-tolerant fold in core/db.AlertClusterStore
// (cluster.go) is free to change cluster MEMBERSHIP because a wrong fold CANNOT silence an incident — but that
// guarantee holds ONLY because the collapse decision here is recomputed from a LIVE per-subject window and
// consumes clusterID solely as a boolean (id > 0), NEVER reading the persisted alert_cluster membership. This
// test PINS that decoupling: it runs the identical correlated + causally-elected input twice with a fake
// ClusterJoin returning two DIFFERENT non-zero ids and asserts the collapse decision — Elected / ElectedRef /
// ElectRule — is IDENTICAL across both. Only the SIGN of clusterID (>0 vs 0) may gate collapse; its VALUE must
// be inert.
//
// If a future "part 2" couples the election to the DB-joined cluster identity, the two runs disagree and this
// test goes RED — the signal to re-examine core/db/cluster.go's foldTarget containment predicate, which that
// coupling would make safety-critical. RED-before/GREEN-after was confirmed by temporarily gating res.Elected
// on clusterID's VALUE instead of its sign (`clusterID < 200`) in correlate.go: run A (id 111) then collapsed
// while run B (id 222) did not, so the Elected-equality assertion below failed. Reverted.
func TestCorrelateActivity_CollapseDecisionIndependentOfClusterIDValue(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	// A 3-host correlated window whose causal root (the hypervisor) is earliest; the SUBJECT is a guest, so a
	// causal election collapses it to evidence (Elected=false) — the load-bearing state this pins.
	window := correlate.Window{Span: 10 * time.Minute, Observations: []correlate.Observation{
		{ExternalRef: "zz-parent", Host: "dc1pve03", SourceType: "librenms", AlertRule: "pve-node-down", At: base},
		{ExternalRef: "guest-a", Host: "dc1vmA", SourceType: "librenms", AlertRule: "guest-down", At: base.Add(time.Minute)},
		{ExternalRef: "guest-b", Host: "dc1vmB", SourceType: "librenms", AlertRule: "guest-down", At: base.Add(2 * time.Minute)},
	}}
	subject := CorrelateInput{ExternalRef: "guest-b", Host: "dc1vmB", AlertRule: "guest-down", Severity: "warning", At: base.Add(2 * time.Minute)}
	topo := causalTopoFake{ind: map[string]int{"dc1pve03": 2}}

	run := func(clusterID int64) CorrelateResult {
		deps := Deps{
			CorrelationWindow: func(context.Context, time.Time) (correlate.Window, error) { return window, nil },
			ClusterJoin: func(context.Context, int64, string, time.Time, time.Duration) (int64, error) {
				return clusterID, nil // the ONLY thing that differs between the two runs
			},
			ClusterTopology: topo,
		}
		res, err := NewActivities(deps).CorrelateActivity(context.Background(), subject)
		if err != nil {
			t.Fatalf("CorrelateActivity (cluster id %d): %v", clusterID, err)
		}
		if res.ClusterID != clusterID {
			t.Fatalf("run with id %d recorded ClusterID %d — the fake did not take effect and the test would be vacuous", clusterID, res.ClusterID)
		}
		return res
	}

	a := run(111)
	b := run(222)

	// Non-vacuity: the two runs genuinely carried DIFFERENT persisted identities...
	if a.ClusterID == b.ClusterID {
		t.Fatalf("both runs recorded ClusterID %d — the test is not exercising two distinct identities", a.ClusterID)
	}
	// ...and the collapse decision was in its load-bearing state (the causal election collapsed the guest).
	if a.Elected {
		t.Fatalf("the causally-elected non-subject guest must collapse (Elected=false) — the invariant is being pinned in the wrong state: %+v", a)
	}
	if a.ElectRule != correlate.ElectRuleIndegree || a.ElectedRef != "zz-parent" {
		t.Fatalf("the causal election must elect the parent by in-degree, got rule=%q elected=%q", a.ElectRule, a.ElectedRef)
	}

	// THE PIN: the collapse decision is INVARIANT to clusterID's VALUE. Only its sign (>0) gates collapse.
	if a.Elected != b.Elected {
		t.Fatalf("collapse decision (Elected) changed with clusterID VALUE: id 111 → %v, id 222 → %v. The "+
			"election has been coupled to the DB cluster identity — re-examine core/db/cluster.go foldTarget, "+
			"now safety-critical", a.Elected, b.Elected)
	}
	if a.ElectedRef != b.ElectedRef {
		t.Fatalf("elected subject changed with clusterID VALUE: id 111 → %q, id 222 → %q — election coupled to the persisted cluster id", a.ElectedRef, b.ElectedRef)
	}
	if a.ElectRule != b.ElectRule {
		t.Fatalf("elect rule changed with clusterID VALUE: id 111 → %q, id 222 → %q — election coupled to the persisted cluster id", a.ElectRule, b.ElectRule)
	}
}
