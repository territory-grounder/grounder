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
)

// ---------------------------------------------------------------------------------------------------------
// THE WAYS AROUND THE APPROVER ADMISSION (spec/015 REQ-1516, TG-254).
//
// vote_admission_test.go proves the straight case: a stranger's approving vote does not release the action.
// An authorization check, though, is only worth what its EDGES are worth, and every one of these is a way a
// non-approver could still decide the fate of a governed action without ever being admitted:
//
//   - deny it (refusing a vote for APPROVAL only, and resolving on a deny, hands a stranger the same veto);
//   - get in FIRST, before the poll exists, on the buffered signal channel;
//   - be refused once and have the poll close, so the real approver's later vote lands on a dead session;
//   - flood the wait until it collapses into something other than a fail-closed stand-down.
//
// Each asserts the WORKFLOW'S terminal outcome plus whether the actuator ran — never that a helper was called.
// ---------------------------------------------------------------------------------------------------------

// scriptedVote is one signal to deliver at a chosen point in the poll's wait.
type scriptedVote struct {
	at      time.Duration
	approve bool
	voter   string
	actID   string // "" = the real sealed action id (so the INV-12 bind passes and ONLY admission can refuse)
}

// voteScriptRun drives the REAL RunnerWorkflow to a POLL_PAUSE poll whose approve_by set is approveBy,
// delivers the whole script of votes, and returns the terminal result, how many times the actuator actually
// ran, and the governance ledger. Mutation is ON with the full interceptor chain wired, so an approval really
// does reach the estate — which is what makes "the actuator never ran" an observation rather than an artifact.
func voteScriptRun(t *testing.T, configured bool, approveBy []string, votes []scriptedVote) (RunnerResult, int, []audit.LedgerEntry) {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	deps := testDeps(
		`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,
		proposeCanaryReversible,
	)
	act := &recordingActuator{}
	sink := &fakeManifestSink{}
	led := audit.NewLedger()
	deps.Ledger = led
	deps.Mutation = safety.NewActuatingChokepoint() // mutation ON (test-only)
	deps.Interceptor = withPermissivePolicy(actuate.NewInterceptor(deps.Mutation, act, audit.NewLedger()))
	deps.Manifests = sink
	deps.ManifestSink = sink
	deps.PostStateObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }
	// Faulted before the effect, quiet after (TG-166b) — an always-quiet reader refuses at the necessity gate.
	deps.ClearObserve = faultedUntilHealed("web01", "NginxDown", act)
	deps.CanaryPinned = func(string, string) (bool, string) { return true, "canary: staged first mutation" }
	deps.ApproveByFor = func(context.Context, ApproveByQuery) []string { return approveBy }
	deps.ApproveByConfigured = configured

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
		t.Fatalf("derive the sealed action id the vote must name: %v", err)
	}
	for _, v := range votes {
		v := v
		id := v.actID
		if id == "" {
			id = actionID
		}
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(VoteSignalName, VoteSignal{Approve: v.approve, Voter: v.voter, ActionID: id})
		}, v.at)
	}

	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-254-edges", SourceID: "prometheus-dc1", AlertRule: "NginxDown",
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
		t.Fatalf("the fixture must reach a POLL_PAUSE poll, band=%q: %+v", res.Band, res)
	}
	return res, act.execs, led.Entries()
}

// ledgerLine renders the ledger for assertions and for a failure message a human can read.
func ledgerLines(entries []audit.LedgerEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Decision+" ["+e.Reason+"]")
	}
	return out
}

// TestANonMembersDenyCannotVetoTheAction closes the half of REQ-1516 that a naive fix misses. If admission
// were applied only to APPROVING votes — or if a refusal RESOLVED the poll instead of continuing the wait —
// then any authenticated operator could still decide the action, just in the other direction: a stranger's
// "deny" (or their mere presence closing the window) would rob the real approver of the decision. A
// non-member has NO authority over this action in either direction.
func TestANonMembersDenyCannotVetoTheAction(t *testing.T) {
	res, execs, led := voteScriptRun(t, true, []string{"user:kyriakos"}, []scriptedVote{
		{at: time.Minute, approve: false, voter: "mallory"},     // the stranger tries to kill it
		{at: 2 * time.Minute, approve: true, voter: "kyriakos"}, // the real approver decides
	})

	if res.Vote != "approved" || execs != 1 {
		t.Fatalf("a non-member's DENY vetoed a governed action the real approver then approved — vote=%q execs=%d: %v",
			res.Vote, execs, ledgerLines(led))
	}
}

// TestARefusedVoteLeavesThePollOpenAndOnTheLedger pins the two properties the refusal branch must have and
// which nothing else asserts: the poll KEEPS WAITING (so the legitimate approver's later vote still counts),
// and the refusal is durably attributed BY VOTER on the hash-chained governance ledger (INV-19) — a stranger
// attempting to release a governed action is a security event, and "why did my approval do nothing?" has to
// be answerable from the ledger rather than by reading code.
func TestARefusedVoteLeavesThePollOpenAndOnTheLedger(t *testing.T) {
	res, execs, led := voteScriptRun(t, true, []string{"user:kyriakos"}, []scriptedVote{
		{at: time.Minute, approve: true, voter: "mallory"},
		{at: 2 * time.Minute, approve: true, voter: "kyriakos"},
	})

	if res.Vote != "approved" || execs != 1 {
		t.Fatalf("a stranger's refused vote closed the poll on the real approver — vote=%q execs=%d: %v",
			res.Vote, execs, ledgerLines(led))
	}
	found := false
	for _, l := range ledgerLines(led) {
		if strings.Contains(l, "human:vote-refused:not-in-approve-by") && strings.Contains(l, "voter=mallory") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the refusal must be on the ledger NAMING the voter, got %v", ledgerLines(led))
	}
}

// TestABufferedPrePollVoteFromAStrangerIsRefused covers the ordering edge. Temporal BUFFERS signals, so a vote
// can be delivered before the session has investigated, classified or gated anything — i.e. before the poll
// (and therefore before approve_by) exists at all. The wait drains that buffer, so the admission must apply to
// a vote that was already sitting in the channel, not only to one that arrives while the selector is parked.
// The control proves buffering itself did not break voting.
func TestABufferedPrePollVoteFromAStrangerIsRefused(t *testing.T) {
	res, execs, led := voteScriptRun(t, true, []string{"user:kyriakos"}, []scriptedVote{{at: 0, approve: true, voter: "mallory"}})
	if res.Vote == "approved" || execs != 0 || res.Mutated {
		t.Fatalf("a vote buffered BEFORE the poll existed released the action — vote=%q execs=%d: %v",
			res.Vote, execs, ledgerLines(led))
	}

	ctrl, ctrlExecs, _ := voteScriptRun(t, true, []string{"user:kyriakos"}, []scriptedVote{{at: 0, approve: true, voter: "kyriakos"}})
	if ctrl.Vote != "approved" || ctrlExecs != 1 {
		t.Fatalf("control: a MEMBER's buffered pre-poll vote must still approve, vote=%q execs=%d", ctrl.Vote, ctrlExecs)
	}
}

// TestAFloodOfRefusedVotesStandsDownFailClosed bounds the abuse. A non-member cannot grow the poll's history
// without limit, and — the direction that matters — the collapse is a DENY, never an approval: the worst a
// stranger can buy with a flood is costing the incident a human re-open.
func TestAFloodOfRefusedVotesStandsDownFailClosed(t *testing.T) {
	var votes []scriptedVote
	for i := 0; i < 70; i++ {
		votes = append(votes, scriptedVote{at: time.Duration(i+1) * time.Second, approve: true, voter: "mallory"})
	}
	res, execs, led := voteScriptRun(t, true, []string{"user:kyriakos"}, votes)

	if res.Vote == "approved" || execs != 0 || res.Mutated {
		t.Fatalf("a flood of refused votes ended in an APPROVAL — vote=%q execs=%d", res.Vote, execs)
	}
	if res.Vote != "refused" {
		t.Fatalf("a sustained non-member flood must stand the poll down naming the abuse, got vote=%q", res.Vote)
	}
	// Bounded: the per-vote ledger writes stop at the cap rather than running for the whole 24h wait.
	if len(led) > 80 {
		t.Fatalf("the refused-vote ledger writes are not bounded: %d entries", len(led))
	}
}

// TestNearMissVoterIdsAreRefused walks the string edges of the membership test. Every one of these is a voter
// id that LOOKS like the approver and must not be treated as them — including the two that echo the approve_by
// grammar itself back at it (an entry names a PRINCIPAL, not a literal to repeat).
//
// Leading/trailing whitespace and letter case are NOT in this list: VoterAdmitted trims and case-folds exactly
// as the law surface does (core/policy.principalIDMatches — verified identical), and the two enforcement
// points disagreeing would be worse than either rule. The voter id is server-derived from the session anyway
// (core/httpapi/vote.go), so it is not attacker-chosen.
func TestNearMissVoterIdsAreRefused(t *testing.T) {
	for _, tc := range []struct{ approveBy, voter string }{
		{"user:kyriakos", "kyriakos2"},
		{"user:kyriakos", "notkyriakos"},
		{"user:kyriakos", "kyriakos@evil.example"},
		{"user:kyriakos", ""},              // an unnamed actor approves nothing
		{"user:kyriakos", "user:kyriakos"}, // echoing the entry's own spelling is not being the principal
		{"group:sre-oncall", "sre-oncall"}, // being NAMED like a group is not being IN it
	} {
		res, execs, _ := voteScriptRun(t, true, []string{tc.approveBy}, []scriptedVote{{at: time.Minute, approve: true, voter: tc.voter}})
		if res.Vote == "approved" || execs != 0 {
			t.Fatalf("approve_by=%q admitted near-miss voter %q — vote=%q execs=%d", tc.approveBy, tc.voter, res.Vote, execs)
		}
	}
}

// TestAnUnconfiguredAdmissionIsCountableOnTheLedger is the LOUDNESS half of oracle 1. Admitting the vote on
// an unconfigured bundle is the right call — refusing would brick every poll on the live estate — but doing
// it SILENTLY is the failure mode this repo keeps repeating: the permitted-because-unconfigured state would
// then be visible only in a boot log nobody re-reads, and nobody could answer "how many actions were
// released with no approver regime in force, and by whom?" from the record.
//
// So every vote that a CONFIGURED bundle would have refused is written to the hash-chained governance ledger
// (INV-19) naming the voter, even though it is permitted. The exposure becomes countable rather than assumed.
func TestAnUnconfiguredAdmissionIsCountableOnTheLedger(t *testing.T) {
	res, execs, led := voteScriptRun(t, false, nil, []scriptedVote{{at: time.Minute, approve: true, voter: "mallory"}})

	if res.Vote != "approved" || execs != 1 {
		t.Fatalf("an unconfigured bundle must leave the vote path as it was — vote=%q execs=%d: %v", res.Vote, execs, ledgerLines(led))
	}
	found := false
	for _, l := range ledgerLines(led) {
		if strings.Contains(l, "human:vote-admitted-unconfigured") && strings.Contains(l, "voter=mallory") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a vote admitted only because NO rule declares approve_by must be recorded by voter, so the exposure is countable rather than silent; got %v", ledgerLines(led))
	}
}

// TestAMisboundVoteFromAMemberDoesNotPreAuthorizeAStranger checks the two refusal counters do not leak into
// each other: a member's vote naming the WRONG action is still ignored on the INV-12 bind, and it must not
// leave the wait in a state where the next (correctly-bound) vote from a STRANGER sails through.
func TestAMisboundVoteFromAMemberDoesNotPreAuthorizeAStranger(t *testing.T) {
	res, execs, led := voteScriptRun(t, true, []string{"user:kyriakos"}, []scriptedVote{
		{at: time.Minute, approve: true, voter: "kyriakos", actID: "an-action-this-session-never-gated"},
		{at: 2 * time.Minute, approve: true, voter: "mallory"},
	})
	if res.Vote == "approved" || execs != 0 || res.Mutated {
		t.Fatalf("a member's misbound vote pre-authorized a stranger's bound one — vote=%q execs=%d: %v",
			res.Vote, execs, ledgerLines(led))
	}
}
