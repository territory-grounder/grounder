package runner

import (
	"context"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// ---------------------------------------------------------------------------------------------------------
// TG-254: "any authenticated operator can approve any governed action."
//
// The vote path bound a vote to the sealed action id (INV-12) and then approved it — full stop. Nothing
// anywhere asked whether the VOTER was permitted to approve. `core/policy.MayApprove` / `VoteAdmission.Admit`
// existed, were tested, and had ZERO production callers, so REQ-1516 was green while the product shipped
// with no approver check at all.
//
// These oracles assert the WORKFLOW'S OUTCOME — did the estate change hands? — not that some helper was
// consulted. Deleting the admission line in workflow.go's vote branch makes the negative case RED (a
// stranger's vote approves and the action EXECUTES); the positive case is what stops "refuse every vote"
// from passing as a fix.
//
// AND THE OTHER DIRECTION, which the first cut of this work got wrong: admission is armed by the BUNDLE, not
// by the action. On a bundle where NO rule declares approve_by — which is what the live deployment runs
// (Semi-auto, 5 rules, 0 approvers) — every poll resolves an EMPTY approver set, so enforcing unconditionally
// refuses approve AND deny from everyone and lands every session on `human:timeout` after VoteWait. That
// trades "any operator can approve anything" for "no operator can approve anything, invisibly", which is
// worse on an estate that actuates. So an unconfigured bundle admits exactly as it always did, and the
// oracle below fails if anyone makes admission unconditional again.
// ---------------------------------------------------------------------------------------------------------

// proposeCanaryReversible is a REVERSIBLE restart of web01 — so it can genuinely execute once approved. The
// poll is forced by the canary pin below rather than by irreversibility, precisely so "no execute occurred"
// is a real observation about the vote and not a side effect of the never-auto floor refusing anyway.
const proposeCanaryReversible = `{"action":"propose","confidence":0.9,"proposal":{"external_ref":"TG-254","target":"web01","op_class":"restart-service","op":"restart","params":{"unit":"nginx"},"reversible":true,"confidence":0.9,"evidence_ids":["tr-1"]}}`

// proposedActionID is the content-hashed id (INV-07) of the action proposeCanaryReversible describes — the
// id a real vote must NAME to get past the INV-12 bind check.
func proposedActionID() (string, error) {
	return manifest.Action{
		Target: "web01", OpClass: "restart-service", Op: "restart",
		Params: map[string]string{"unit": "nginx"}, Reversible: true,
	}.ID()
}

// voteAdmissionRun drives the REAL RunnerWorkflow to a POLL_PAUSE poll whose approve_by set is approveBy,
// delivers one vote from voter, and reports the terminal result plus how many times the actuator actually
// ran. Mutation is ON and the actuation chain is fully wired, so an approval really does reach the estate —
// which is what makes "no execute occurred" mean something.
//
// `configured` is the BUNDLE fact the worker's composition root resolves from the active ruleset (does ANY
// rule declare approve_by?). It is a separate parameter from approveBy precisely because the two are
// independent: a bundle can declare approvers on one rule while THIS action's matched rule declares none.
func voteAdmissionRun(t *testing.T, configured bool, approveBy []string, voter string) (RunnerResult, int) {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	investigateThenPropose := []string{
		`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,
		proposeCanaryReversible,
	}
	deps := testDeps(investigateThenPropose...)
	gate := safety.NewActuatingChokepoint() // mutation ON (test-only) — an approved action really executes
	act := &recordingActuator{}
	sink := &fakeManifestSink{}
	deps.Mutation = gate
	deps.Interceptor = withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger()))
	deps.Manifests = sink
	deps.ManifestSink = sink
	deps.PostStateObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }
	// Faulted before the effect, quiet after (TG-166b) — an always-quiet reader refuses at the necessity gate.
	deps.ClearObserve = faultedUntilHealed("web01", "NginxDown", act)
	// A canary pin forces POLL_PAUSE on a REVERSIBLE action (spec/001 REQ-009), so the session parks on a
	// human vote for an action the actuation chain would otherwise be willing to run.
	deps.CanaryPinned = func(string, string) (bool, string) { return true, "canary: staged first mutation" }
	// THE SEAM UNDER TEST: the composition root's answers to "who may approve this poll?" and "does this
	// bundle declare an approver regime at all?", both returned by the gate ACTIVITY (so they land in
	// history) rather than computed in workflow code.
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
	// The vote NAMES the sealed action (INV-12) — so the action-id bind check passes and the ONLY thing that
	// can still refuse it is approver admission. Without this the test would prove the old check, not the new one.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(VoteSignalName, VoteSignal{Approve: true, Voter: voter, ActionID: actionID})
	}, time.Minute)

	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-254", SourceID: "prometheus-dc1", AlertRule: "NginxDown",
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
		t.Fatalf("the fixture must reach a POLL_PAUSE poll (else nothing is being voted on), band=%q: %+v", res.Band, res)
	}
	return res, act.execs
}

// ORACLE 1 — TestAnUnconfiguredBundleAdmitsTheVoteExactlyAsBefore is the anti-BRICKING oracle, and the one
// that guards the LIVE deployment. Its bundle declares no approve_by anywhere, so every poll resolves an
// EMPTY approver set; a non-member's vote must therefore approve and execute EXACTLY as it did before this
// control existed. Making admission unconditional (the killing mutation) turns this RED: mallory's vote is
// refused, the poll parks until VoteWait and lands on `human:timeout`, and the incident is never actioned —
// which is what would happen to every POLL_PAUSE session on the live estate (Semi-auto, 5 rules, 0 declaring
// approve_by) the moment such a build merged.
//
// This is not a hole being blessed. It is today's behaviour preserved, made countable on the ledger
// (human:vote-admitted-unconfigured) and shouted at boot, until an operator declares an approver regime.
func TestAnUnconfiguredBundleAdmitsTheVoteExactlyAsBefore(t *testing.T) {
	res, execs := voteAdmissionRun(t, false, nil, "mallory")

	if res.Vote != "approved" {
		t.Fatalf("a bundle that declares NO approve_by must leave the vote path exactly as it was — instead the vote did not approve (vote=%q), which is every poll on the live deployment bricked into human:timeout: %+v", res.Vote, res)
	}
	if execs != 1 {
		t.Fatalf("the admitted vote must still reach the estate exactly once, actuator ran %d time(s): %+v", execs, res)
	}
}

// ORACLE 2 — TestAVoteFromOutsideApproveByNeverReleasesTheAction is the TG-254 oracle: under a CONFIGURED
// bundle, a correctly-bound, authenticated approving vote from a principal who is NOT in the poll's
// approve_by set must NOT approve and must NOT execute. Deleting the admission branch in workflow.go turns
// this RED — mallory's vote approves and the actuator runs.
func TestAVoteFromOutsideApproveByNeverReleasesTheAction(t *testing.T) {
	res, execs := voteAdmissionRun(t, true, []string{"group:sre-oncall"}, "mallory")

	if res.Vote == "approved" {
		t.Fatalf("a vote from outside approve_by APPROVED the action — any authenticated operator can approve anything (TG-254): %+v", res)
	}
	if execs != 0 || res.Mutated {
		t.Fatalf("a refused vote must never reach the estate: actuator ran %d time(s), mutated=%v: %+v", execs, res.Mutated, res)
	}
	// The refusal is recorded and the poll stands down fail-closed rather than silently approving.
	if res.Vote != "timeout" && res.Vote != "refused" {
		t.Fatalf("a refused vote must leave the poll unresolved (it may not deny on a stranger's behalf either), got vote=%q: %+v", res.Vote, res)
	}
}

// ORACLE 3 — TestAVoteFromInsideApproveByStillApproves is the negative control. Without it, "refuse every
// vote" — or simply breaking the vote path — would pass the oracle above while destroying governed autonomy.
func TestAVoteFromInsideApproveByStillApproves(t *testing.T) {
	res, execs := voteAdmissionRun(t, true, []string{"group:sre-oncall", "user:kyriakos"}, "kyriakos")

	if res.Vote != "approved" {
		t.Fatalf("an approve_by MEMBER's vote must approve the action, got vote=%q: %+v", res.Vote, res)
	}
	if execs != 1 {
		t.Fatalf("an approved action must actually execute exactly once, actuator ran %d time(s): %+v", execs, res)
	}
}

// ORACLE 4 — TestAConfiguredBundleRefusesAnActionThatNamesNoApproverOfItsOwn is the SUBTLE one, and it is
// what proves "configured" is a property of the BUNDLE rather than of the action being voted on. Here the
// operator HAS declared an approver regime (some other rule in the bundle names approvers — the fact
// cmd/worker's rulesDeclaringApprovers resolves, see TestConfiguredIsABundlePropertyNotAnActionProperty),
// but THIS action's matched rule declares nobody. Under a declared regime that emptiness is an answer, not a
// silence: nobody may approve this action, and the vote is refused.
//
// It is the exact inverse of oracle 1 with the SAME empty approver set — the only difference between them is
// the bundle fact. If someone collapses the two (by deriving "configured" from the per-action set, or by
// dropping the flag), one of this pair goes red whichever way they collapse it.
func TestAConfiguredBundleRefusesAnActionThatNamesNoApproverOfItsOwn(t *testing.T) {
	res, execs := voteAdmissionRun(t, true, nil, "kyriakos")

	if res.Vote == "approved" || execs != 0 || res.Mutated {
		t.Fatalf("under a bundle that DOES declare approvers, an action whose own rule names none must be approvable by NOBODY (an empty set must not read as 'anyone'): vote=%q execs=%d: %+v", res.Vote, execs, res)
	}
}

// TestVoterAdmittedIsFailClosed pins the pure membership rule the workflow replays on, including the two
// cases whose direction is the whole point: an empty set and an unresolvable group entry both admit nobody.
func TestVoterAdmittedIsFailClosed(t *testing.T) {
	cases := []struct {
		name      string
		approveBy []string
		voter     string
		want      bool
	}{
		{"empty set admits nobody", nil, "kyriakos", false},
		{"blank entries admit nobody", []string{"", "   ", "user:"}, "kyriakos", false},
		{"empty voter is admitted by nothing", []string{"user:kyriakos"}, "  ", false},
		{"named user is admitted", []string{"user:kyriakos"}, "kyriakos", true},
		{"named user matches case-insensitively", []string{"user:Kyriakos"}, "kyriakos", true},
		{"unprefixed entry matches the id", []string{"kyriakos"}, "kyriakos", true},
		{"a non-member is refused", []string{"user:alice", "group:sre-oncall"}, "mallory", false},
		// A group entry needs an identity backend to expand; workflow code has none, so it admits nobody
		// rather than guessing. The composition root expands groups at gate time when a resolver is wired.
		{"an unexpanded group entry admits nobody", []string{"group:sre-oncall"}, "sre-oncall", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VoterAdmitted(tc.approveBy, tc.voter); got != tc.want {
				t.Fatalf("VoterAdmitted(%q, %q) = %v, want %v", tc.approveBy, tc.voter, got, tc.want)
			}
		})
	}
}
