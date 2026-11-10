package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
	"github.com/territory-grounder/grounder/temporal/wfauth"
	"github.com/territory-grounder/grounder/temporal/wfauth/wfauthtest"
)

// TG-660 — a vote is a CLAIM until its envelope verifies. The waiting poll ignores (and ledgers) an unsigned
// vote and releases on a signed one; the activity refuses a replay of a spent envelope.
//
// KILLING MUTATION (executed 2026-11-07): dropping the `if voteAuthActive { ... }` block from the vote loop turned
// TestAnUnsignedVoteIsIgnoredAndASignedOneReleases RED — the unsigned vote released the action.

// voteAuthRun drives the vote_admission rig with a REAL verifier and a scripted sequence of votes.
func voteAuthRun(t *testing.T, votes func(actionID string) []VoteSignal) (RunnerResult, int, *captureLedger) {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(
		`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,
		proposeCanaryReversible,
	)
	gate := safety.NewActuatingChokepoint()
	act := &recordingActuator{}
	sink := &fakeManifestSink{}
	led := &captureLedger{}
	deps.Mutation = gate
	deps.Interceptor = withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger()))
	deps.Manifests, deps.ManifestSink = sink, sink
	deps.Ledger = audit.NewLedger().WithSink(led)
	deps.PostStateObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }
	deps.ClearObserve = faultedUntilHealed("web01", "NginxDown", act)
	deps.CanaryPinned = func(string, string) (bool, string) { return true, "canary: staged first mutation" }
	_, v := wfauthtest.Pair(t)
	deps.WorkflowAuth = v // the REAL verifier, replacing the permissive default testDeps installs

	acts := NewActivities(deps)
	registerAll(env, acts)
	env.RegisterActivity(acts.BackfillManifestActivity)
	env.RegisterActivity(acts.ObserveClearedActivity)
	env.RegisterActivity(acts.RecoveredSinceActivity)
	env.RegisterActivity(acts.ReconcileActivity)
	env.RegisterActivity(acts.RecordPendingActivity)
	env.RegisterActivity(acts.ResolvePendingActivity)

	actionID, err := proposedActionID()
	if err != nil {
		t.Fatalf("derive the sealed action id: %v", err)
	}
	for i, vs := range votes(actionID) {
		vs := vs
		env.RegisterDelayedCallback(func() { env.SignalWorkflow(VoteSignalName, vs) }, time.Duration(i+1)*time.Minute)
	}
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-660-vote", SourceID: "prometheus-dc1", AlertRule: "NginxDown",
		Host: "web01", Severity: ingest.SeverityWarning, Site: "dc1",
	})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("read the workflow result: %v", err)
	}
	if res.Band != safety.BandPollPause.String() {
		t.Fatalf("the fixture must reach a POLL_PAUSE poll, band=%q", res.Band)
	}
	return res, act.execs, led
}

func signedVote(t *testing.T, externalRef, actionID string, approve bool, voter string) VoteSignal {
	t.Helper()
	return VoteSignal{Approve: approve, Voter: voter, ActionID: actionID,
		Auth: wfauthtest.Sign(t, wfauth.VoteKind, wfauth.VoteFields(externalRef, actionID, approve, voter)...)}
}

func TestAnUnsignedVoteIsIgnoredAndASignedOneReleases(t *testing.T) {
	res, execs, led := voteAuthRun(t, func(id string) []VoteSignal {
		return []VoteSignal{
			{Approve: true, Voter: "mallory", ActionID: id},    // unsigned: anyone on the backplane
			signedVote(t, "TG-660-vote", id, true, "kyriakos"), // the grounder's signed vote
		}
	})
	if res.Vote != "approved" || execs != 1 {
		t.Fatalf("the SIGNED vote must release the action (vote=%q execs=%d): %+v", res.Vote, execs, res)
	}
	if !led.has(wfauth.RefusalDecision) {
		t.Fatalf("the unsigned vote must be ledgered as %s; ledger=%v", wfauth.RefusalDecision, decisions(led))
	}
	for _, e := range led.entries {
		if e.Decision == wfauth.RefusalDecision && !strings.Contains(e.Reason, "actor=mallory why=unsigned") {
			t.Fatalf("the refusal must name the claimed voter and the class: %q", e.Reason)
		}
	}
}

// A vote signed for ANOTHER session does not release this one: the session's external_ref is part of what is signed.
func TestAVoteSignedForAnotherSessionDoesNotRelease(t *testing.T) {
	res, execs, led := voteAuthRun(t, func(id string) []VoteSignal {
		return []VoteSignal{signedVote(t, "TG-some-other-session", id, true, "kyriakos")}
	})
	if res.Vote == "approved" || execs != 0 {
		t.Fatalf("a vote minted for another session released this one: vote=%q execs=%d", res.Vote, execs)
	}
	if !led.has(wfauth.RefusalDecision) {
		t.Fatalf("the cross-session vote must be ledgered as a refusal; ledger=%v", decisions(led))
	}
}

// At the activity: the same envelope presented twice is a replay — refused, ledgered.
func TestVerifyVoteAuthRefusesAReplayedEnvelope(t *testing.T) {
	_, v := wfauthtest.Pair(t)
	led := &captureLedger{}
	a := NewActivities(Deps{WorkflowAuth: v, Ledger: audit.NewLedger().WithSink(led)})
	vote := signedVote(t, "TG-r", "act-1", true, "kyriakos")
	in := VerifyVoteAuthInput{ExternalRef: "TG-r", Vote: vote}
	first, err := a.VerifyVoteAuthActivity(context.Background(), in)
	if err != nil || !first.OK {
		t.Fatalf("first presentation must verify: %+v %v", first, err)
	}
	second, err := a.VerifyVoteAuthActivity(context.Background(), in)
	if err != nil || second.OK || second.Class != "replayed" {
		t.Fatalf("second presentation must be refused as replayed: %+v %v", second, err)
	}
	if !led.has(wfauth.RefusalDecision) {
		t.Fatalf("the replay must be ledgered; ledger=%v", decisions(led))
	}
	// nil verifier (no key) refuses everything, honestly classed.
	none := NewActivities(Deps{Ledger: audit.NewLedger().WithSink(&captureLedger{})})
	res, err := none.VerifyVoteAuthActivity(context.Background(), in)
	if err != nil || res.OK || res.Class != "no-key" {
		t.Fatalf("a worker with no key must refuse every vote as no-key: %+v %v", res, err)
	}
}
