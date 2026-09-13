package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/risk"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// ---------------------------------------------------------------------------------------------------------
// TG-559 / spec/012 REQ-1127 — the autonomous fallback approver.
//
// The pure half proves the DECISION: a missing first look alone ⇒ act; any other reason, even one the
// classifier never got to record, ⇒ hold. The workflow half proves the OUTCOME the owner asked for: an armed
// deployment neither posts a poll nor parks for VoteWait, and an unarmed one is exactly the old vote path.
// ---------------------------------------------------------------------------------------------------------

// fbaAutoInput is a reversible, prediction-backed restart that the classifier admits AUTO when nothing else is
// set — the baseline every row below perturbs, so each row isolates exactly the input it names.
func fbaAutoInput() risk.GatedInput {
	return risk.GatedInput{OpClass: "restart-service", Reversible: risk.Reversible, HasPrediction: true}
}

// TestFallbackApproverBaselineClassifiesAuto is the vacuity control for the tables below: if the baseline
// itself polled, "clearing the first-look inputs stops the poll" could never be true and every hold row would
// pass for the wrong reason.
func TestFallbackApproverBaselineClassifiesAuto(t *testing.T) {
	if d := risk.Classify(fbaAutoInput()); d.Band != safety.BandAuto {
		t.Fatalf("the fixture baseline must classify AUTO, got %s (%v)", d.Band, d.Signals)
	}
	if r := resolvePollAutonomously(fbaAutoInput(), risk.Classify(fbaAutoInput())); r.Resolved {
		t.Fatalf("a decision that did not poll needs no approver, got %+v", r)
	}
}

// TestFallbackApproverActsWhenOnlyAFirstLookWasMissing: each first-look input ALONE polls, and the fallback
// approver acts on it — naming the reason that asked.
func TestFallbackApproverActsWhenOnlyAFirstLookWasMissing(t *testing.T) {
	rows := []struct {
		reason string
		set    func(*risk.GatedInput)
	}{
		{"canary-policy-pinned", func(g *risk.GatedInput) { g.CanaryPinned = true }},
		{"op-class-not-graduated", func(g *risk.GatedInput) { g.UngraduatedClass = true }},
		{"ood-novel-incident", func(g *risk.GatedInput) { g.NovelIncident = true }},
	}
	for _, row := range rows {
		gi := fbaAutoInput()
		row.set(&gi)
		polled := risk.Classify(gi)
		if polled.Band != safety.BandPollPause || polled.Signals["poll_reason"] != row.reason {
			t.Fatalf("fixture: %s must poll with that reason, got %s %v", row.reason, polled.Band, polled.Signals)
		}
		r := resolvePollAutonomously(gi, polled)
		if !r.Resolved || !r.Approve || r.Key != row.reason {
			t.Errorf("%s alone is a missing first look and must ACT naming it, got %+v", row.reason, r)
		}
	}
}

// TestFallbackApproverHoldsWhatAFirstLookCannotClear is the oracle for the design choice. Every row adds a
// hold-worthy input TOGETHER WITH a canary pin. The classifier checks the canary pin BEFORE actor attribution,
// rationale, prediction, deviation, high-risk and evidence — so for those rows the RECORDED poll_reason is
// "canary-policy-pinned", and a resolver keyed on the recorded reason would ACT on a person's deliberate change,
// an unpredicted action, or a host TG just broke. Swap resolvePollAutonomously for a recorded-reason lookup and
// these rows go red.
func TestFallbackApproverHoldsWhatAFirstLookCannotClear(t *testing.T) {
	rows := []struct {
		name    string
		remains string
		set     func(*risk.GatedInput)
	}{
		{"a person deliberately made the change", "actor-attributed-authorized", func(g *risk.GatedInput) { g.AttributionStandDown = true }},
		{"an unsanctioned actor touched the target", "actor-attributed-suspicious", func(g *risk.GatedInput) { g.AttributionSecurity = true }},
		{"who changed the target is unresolved", "actor-attribution-escalate", func(g *risk.GatedInput) { g.AttributionEscalate = true }},
		{"the action is irreversible", "irreversible-or-never-auto-floor", func(g *risk.GatedInput) { g.Reversible = risk.Irreversible }},
		{"the command is destructive", "server-derived-destructive-op", func(g *risk.GatedInput) { g.ServerDestructive = true }},
		{"the input carried a jailbreak", "jailbreak-detected", func(g *risk.GatedInput) { g.Jailbreak = true }},
		{"a data-bearing workload is mutated", "stateful-workload-mutation", func(g *risk.GatedInput) {
			g.StatefulTarget = true
			g.Reversible = risk.ReversibleMixed
		}},
		{"TG would restart its own control plane", "self-protected-control-plane-restart", func(g *risk.GatedInput) { g.SelfProtectedRestart = true }},
		{"the rationale names a different host", "rationale-names-a-different-host", func(g *risk.GatedInput) { g.RationaleHostMismatch = true }},
		{"there is no committed prediction", "no-committed-prediction", func(g *risk.GatedInput) { g.HasPrediction = false }},
		{"TG's last same-family action here deviated", "verdict-deviation-or-invalid", func(g *risk.GatedInput) {
			g.HasVerdict = true
			g.Verdict = safety.VerdictDeviation
		}},
		{"a high-risk alert category", "high-risk-category-default", func(g *risk.GatedInput) { g.HighRiskCategory = true }},
		{"an auto-resolve claim with no bound evidence", "auto-resolve-evidence-unbound", func(g *risk.GatedInput) {
			g.SilentCognitionGuard = true
			g.AutoResolveMarked = true
		}},
		{"a platform pack's never-auto floor", "band-floor-pack-cisco", func(g *risk.GatedInput) {
			g.BandFloor, g.BandFloorApplies, g.BandFloorReason = safety.BandPollPause, true, "pack-cisco"
		}},
	}
	for _, row := range rows {
		gi := fbaAutoInput()
		gi.CanaryPinned = true
		row.set(&gi)
		polled := risk.Classify(gi)
		if polled.Band != safety.BandPollPause {
			t.Fatalf("fixture %q must poll, got %s", row.name, polled.Band)
		}
		r := resolvePollAutonomously(gi, polled)
		if !r.Resolved || r.Approve {
			t.Errorf("%s: the fallback approver ACTED (recorded reason %q) — it must hold: %+v", row.name, polled.Signals["poll_reason"], r)
			continue
		}
		if r.Key != row.remains {
			t.Errorf("%s: the hold must name the reason that remains, got %q want %q", row.name, r.Key, row.remains)
		}
	}
}

// TestFallbackApproverNeverRewritesTheRecordedDecision: the counterfactual runs on a PRIVATE signals copy and the
// resolution is written onto a FRESH map. Sharing either map would overwrite the poll_reason the audit trail
// shows, or mutate the risk-audit row already appended to the hash-chained ledger.
func TestFallbackApproverNeverRewritesTheRecordedDecision(t *testing.T) {
	gi := fbaAutoInput()
	gi.CanaryPinned = true
	gi.AttributionStandDown = true
	gi.Signals = map[string]string{"actor_attribution": "attributed-authorized"}
	polled := risk.Classify(gi) // polled.Signals IS gi.Signals — the classifier writes into the map it is handed
	asked := polled.Signals["poll_reason"]

	r := resolvePollAutonomously(gi, polled)
	if got := gi.Signals["poll_reason"]; got != asked {
		t.Fatalf("the counterfactual overwrote the recorded poll_reason: %q → %q", asked, got)
	}
	d := polled
	recordAutonomousResolution(&d, r)
	if _, leaked := gi.Signals[signalAutonomousResolution]; leaked {
		t.Fatal("the resolution was written into the map the audit row shares — it must land on a fresh map")
	}
	if d.Signals[signalAutonomousResolution] != "hold" || d.Signals["actor_attribution"] != "attributed-authorized" {
		t.Fatalf("the recorded decision must carry the resolution AND keep its existing signals, got %v", d.Signals)
	}
}

// TestFallbackApproverResolutionRoundTripsThroughHistory: what the activity records is exactly what the
// workflow reads, and nothing but the two recorded words resolves anything.
func TestFallbackApproverResolutionRoundTripsThroughHistory(t *testing.T) {
	for _, want := range []AutonomousResolution{
		{Resolved: true, Approve: true, Key: "canary-policy-pinned", Why: "Acting: x"},
		{Resolved: true, Key: "actor-attributed-authorized", Why: "Held: y"},
	} {
		d := risk.Decision{Band: safety.BandPollPause}
		recordAutonomousResolution(&d, want)
		if got := autonomousResolutionOf(d); got != want {
			t.Errorf("round trip: recorded %+v, read back %+v", want, got)
		}
	}
	for _, v := range []string{"", "approve", "ACT", "maybe"} {
		d := risk.Decision{Signals: map[string]string{signalAutonomousResolution: v}}
		if got := autonomousResolutionOf(d); got.Resolved {
			t.Errorf("%q must not resolve a poll, got %+v", v, got)
		}
	}
}

// TestRollbackFallbackActsOnlyOnAReversibleInverse pins the rollback row of the ruling.
func TestRollbackFallbackActsOnlyOnAReversibleInverse(t *testing.T) {
	if r := resolveRollbackAutonomously(true); !r.Resolved || !r.Approve {
		t.Errorf("an operator-requested undo with a reversible inverse must act, got %+v", r)
	}
	if r := resolveRollbackAutonomously(false); !r.Resolved || r.Approve {
		t.Errorf("an inverse that is not reversible must be held, got %+v", r)
	}
}

// TestSealRollbackRecordsTheFallbackDecisionOnlyWhenArmed: the rollback lane's resolution is produced by the
// seal ACTIVITY (history), and an unarmed deployment records none.
func TestSealRollbackRecordsTheFallbackDecisionOnlyWhenArmed(t *testing.T) {
	in := RollbackInput{
		ForwardActionID: "forward-tg559", ForwardOpClass: "start-service", ForwardOp: "start",
		ForwardTarget: "app01", ForwardParams: map[string]string{"unit": "nginx"}, ForwardReversible: true,
		RollbackExternalRef: "rollback:forward-tg559",
	}
	for _, armed := range []bool{true, false} {
		acts := NewActivities(Deps{ManifestSink: &captureSink{}, AutonomousResolver: armed})
		out, err := acts.SealRollbackActivity(context.Background(), in)
		if err != nil || !out.Sealed {
			t.Fatalf("armed=%v: the seal must succeed for a reversible forward: %+v %v", armed, out, err)
		}
		if armed && !(out.Autonomous.Resolved && out.Autonomous.Approve) {
			t.Errorf("armed: the seal must record an ACT for a reversible inverse, got %+v", out.Autonomous)
		}
		if !armed && out.Autonomous.Resolved {
			t.Errorf("unarmed: the seal must record no resolution (the human vote applies), got %+v", out.Autonomous)
		}
	}
}

// proposeFallbackIrreversible is the canary fixture's restart declared NOT reversible — the never-auto floor
// polls it, and no first look can clear that.
const proposeFallbackIrreversible = `{"action":"propose","confidence":0.9,"proposal":{"external_ref":"TG-559","target":"web01","op_class":"restart-service","op":"restart","params":{"unit":"nginx"},"reversible":false,"confidence":0.9,"evidence_ids":["tr-1"]}}`

type fallbackApproverOutcome struct {
	res     RunnerResult
	execs   int
	notices []notifier.Notice
	ledger  []audit.LedgerEntry
	elapsed time.Duration
}

// fallbackApproverRun drives the REAL RunnerWorkflow with mutation ON and a canary pin (so a reversible
// proposal polls), armed or not, and delivers NO vote. What happens next is the whole question.
func fallbackApproverRun(t *testing.T, armed bool, proposal string) fallbackApproverOutcome {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`, proposal)
	gate := safety.NewActuatingChokepoint() // mutation ON (test-only) — an approval really reaches the estate
	act := &recordingActuator{}
	sink := &fakeManifestSink{}
	deps.Mutation = gate
	deps.Interceptor = withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger()))
	deps.Manifests = sink
	deps.ManifestSink = sink
	deps.PostStateObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }
	deps.ClearObserve = faultedUntilHealed("web01", "NginxDown", act)
	deps.CanaryPinned = func(string, string) (bool, string) { return true, "canary: staged first mutation" }
	deps.AutonomousResolver = armed
	var notices []notifier.Notice
	deps.Notify = func(_ context.Context, n notifier.Notice) error {
		notices = append(notices, n)
		return nil
	}

	acts := NewActivities(deps)
	registerAll(env, acts)
	env.RegisterActivity(acts.BackfillManifestActivity)
	env.RegisterActivity(acts.ObserveClearedActivity)
	env.RegisterActivity(acts.RecoveredSinceActivity)
	env.RegisterActivity(acts.ReconcileActivity)
	env.RegisterActivity(acts.RecordPendingActivity)
	env.RegisterActivity(acts.ResolvePendingActivity)

	start := env.Now()
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-559", SourceID: "prometheus-dc1", AlertRule: "NginxDown",
		Host: "web01", Severity: ingest.SeverityWarning, Site: "dc1",
	})
	elapsed := env.Now().Sub(start)
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("read the workflow result: %v", err)
	}
	if res.Band != safety.BandPollPause.String() {
		t.Fatalf("the fixture must reach POLL_PAUSE (else nothing is being decided), band=%q: %+v", res.Band, res)
	}
	return fallbackApproverOutcome{res: res, execs: act.execs, notices: notices, ledger: deps.Ledger.Entries(), elapsed: elapsed}
}

func fallbackLedgerHas(entries []audit.LedgerEntry, prefix string) bool {
	for _, e := range entries {
		if strings.HasPrefix(e.Decision, prefix) {
			return true
		}
	}
	return false
}

func fallbackPolled(notices []notifier.Notice) bool {
	for _, n := range notices {
		if n.Approval || len(n.Choices) > 0 {
			return true
		}
	}
	return false
}

// TestArmedFallbackApproverActsOnACanaryPinWithoutPollingOrWaiting — the owner's ask, end to end: a reversible
// action held back only by a canary pin EXECUTES, nobody is polled, and the session does not park for VoteWait.
func TestArmedFallbackApproverActsOnACanaryPinWithoutPollingOrWaiting(t *testing.T) {
	o := fallbackApproverRun(t, true, proposeCanaryReversible)
	if o.res.Vote != "autonomous-approved" || o.execs != 1 {
		t.Fatalf("the fallback approver must approve and the action must execute once: vote=%q execs=%d: %+v", o.res.Vote, o.execs, o.res)
	}
	if fallbackPolled(o.notices) {
		t.Errorf("no poll may be posted when TG decides, got notices %+v", o.notices)
	}
	if !fallbackLedgerHas(o.ledger, "fallback:autonomous-approve:canary-policy-pinned") {
		t.Error("the approval must be ledger-recorded as the fallback approver's, naming the reason")
	}
	if o.elapsed >= VoteWait {
		t.Errorf("the session parked for the vote wait (%s) — a decided poll must not wait", o.elapsed)
	}
}

// TestArmedFallbackApproverHoldsAnIrreversibleProposalWithoutPollingOrWaiting — "hold + tell me": nothing
// executes, nobody is polled, the hold is recorded with its reason, and the session does not park.
func TestArmedFallbackApproverHoldsAnIrreversibleProposalWithoutPollingOrWaiting(t *testing.T) {
	o := fallbackApproverRun(t, true, proposeFallbackIrreversible)
	if o.res.Vote != "autonomous-held" || o.execs != 0 || o.res.Mutated {
		t.Fatalf("an irreversible proposal must be held unexecuted: vote=%q execs=%d mutated=%v", o.res.Vote, o.execs, o.res.Mutated)
	}
	if fallbackPolled(o.notices) {
		t.Errorf("no poll may be posted when TG decides, got notices %+v", o.notices)
	}
	if len(o.notices) == 0 {
		t.Error("a hold must still TELL someone — no notice was sent")
	}
	if !fallbackLedgerHas(o.ledger, "fallback:autonomous-hold:irreversible-or-never-auto-floor") {
		t.Error("the hold must be ledger-recorded naming the reason that remains")
	}
	if o.elapsed >= VoteWait {
		t.Errorf("the session parked for the vote wait (%s) — a decided poll must not wait", o.elapsed)
	}
}

// TestUnarmedFallbackApproverLeavesTheHumanVotePathInCharge is the dark-by-default control: the SAME reversible
// canary proposal on an unarmed deployment posts a poll, waits the full VoteWait, times out to DENY, and records
// nothing as the fallback approver. Arming by default turns this red.
func TestUnarmedFallbackApproverLeavesTheHumanVotePathInCharge(t *testing.T) {
	o := fallbackApproverRun(t, false, proposeCanaryReversible)
	if o.res.Vote != "timeout" || o.execs != 0 {
		t.Fatalf("unarmed, an unanswered poll must time out without executing: vote=%q execs=%d", o.res.Vote, o.execs)
	}
	if !fallbackPolled(o.notices) {
		t.Error("unarmed, the poll must still be posted")
	}
	if o.elapsed < VoteWait {
		t.Errorf("unarmed, the session must wait the full vote window, waited %s", o.elapsed)
	}
	if fallbackLedgerHas(o.ledger, "fallback:") {
		t.Error("unarmed, nothing may be recorded as the fallback approver")
	}
}
