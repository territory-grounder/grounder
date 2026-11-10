package main

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/territory-grounder/grounder/core/credential"

	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// THE ALIVENESS ORACLE for the approve_by wiring (spec/015 REQ-1516, TG-254).
//
// It drives approveByFor — the SAME function main() installs as runner.Deps.ApproveByFor — against a REAL
// policy.Engine over a REAL rule bundle. The unit tests for MayApprove proved the rule; what shipped broken
// was the WIRE, so the oracle drives the wire.

// approveByEngine builds a real engine over one rule with the given approvers.
func approveByEngine(t *testing.T, approveBy []string) *policy.Engine {
	t.Helper()
	r, err := policy.NewRule(policy.Rule{
		ID:        "poll-restart-service",
		Match:     policy.Match{OpClass: "restart-service"},
		Verdict:   policy.VerdictApprove,
		ApproveBy: approveBy,
	})
	if err != nil {
		t.Fatalf("build the rule: %v", err)
	}
	eng, err := policy.NewEngine(context.Background(), policy.RuleSet{Rules: []policy.Rule{r}})
	if err != nil {
		t.Fatalf("build the engine: %v", err)
	}
	return eng
}

var approveByProbe = runner.ApproveByQuery{
	OpClass: "restart-service", Op: "restart", Host: "web01", Reversible: true,
	Band: safety.BandPollPause, Site: "dc1", ActionID: "act-1", ExternalRef: "TG-254",
}

// TestApproveByForCarriesTheGoverningRulesApprovers is the positive leg: the operator's declared approvers
// reach the runner's gate unchanged, so the poll is votable by exactly the people the rule names.
func TestApproveByForCarriesTheGoverningRulesApprovers(t *testing.T) {
	got := approveByFor(context.Background(), approveByEngine(t, []string{"group:sre-oncall", "user:alice"}),
		nil, func() policy.Mode { return policy.ModeShadow }, approveByProbe)

	if len(got) != 2 || got[0] != "group:sre-oncall" || got[1] != "user:alice" {
		t.Fatalf("the governing rule's approve_by must reach the gate unchanged, got %v", got)
	}
}

// TestApproveByForFailsClosedWithNoApproversDeclared is the direction that matters. Every one of these
// deployment states must produce an EMPTY set — which the runner reads as "nobody may approve", never
// "anybody may approve". The permissive reading is precisely the TG-254 defect.
func TestApproveByForFailsClosedWithNoApproversDeclared(t *testing.T) {
	shadow := func() policy.Mode { return policy.ModeShadow }
	cases := []struct {
		name string
		eng  approveByDecider
	}{
		{"no policy engine wired at all", nil},
		{"the governing rule declares no approve_by", approveByEngine(t, nil)},
		{"the engine cannot answer", failingDecider{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := approveByFor(context.Background(), tc.eng, nil, shadow, approveByProbe); len(got) != 0 {
				t.Fatalf("must resolve to an EMPTY approver set (fail closed), got %v", got)
			}
		})
	}
}

// TestApproveByForNamesNobodyForAnActionPolicyDenies proves a REFUSED action does not become approvable: a
// deny short-circuits and carries no approvers, so no operator can vote it through the poll lane.
func TestApproveByForNamesNobodyForAnActionPolicyDenies(t *testing.T) {
	deny, err := policy.NewRule(policy.Rule{
		ID: "never-this", Match: policy.Match{OpClass: "restart-service"}, Verdict: policy.VerdictDeny,
	})
	if err != nil {
		t.Fatalf("build the deny rule: %v", err)
	}
	allow, err := policy.NewRule(policy.Rule{
		ID: "poll-restart-service", Match: policy.Match{OpClass: "restart-service"},
		Verdict: policy.VerdictApprove, ApproveBy: []string{"user:alice"},
	})
	if err != nil {
		t.Fatalf("build the approve rule: %v", err)
	}
	eng, err := policy.NewEngine(context.Background(), policy.RuleSet{Rules: []policy.Rule{allow, deny}})
	if err != nil {
		t.Fatalf("build the engine: %v", err)
	}

	got := approveByFor(context.Background(), eng, nil, func() policy.Mode { return policy.ModeShadow }, approveByProbe)
	if len(got) != 0 {
		t.Fatalf("a policy-DENIED action must name no approver (it must not be votable through), got %v", got)
	}
}

// TestConfiguredIsABundlePropertyNotAnActionProperty is the composition-root half of oracle 4. The runner's
// TestAConfiguredBundleRefusesAnActionThatNamesNoApproverOfItsOwn asserts what a CONFIGURED bundle does to an
// action whose own rule names nobody; this asserts that such a bundle is what the shipped detection actually
// produces — that one rule declaring approve_by arms enforcement for EVERY rule in the bundle, including the
// ones that declare none. Without this, the runner oracle would be asserting against a premise no code holds.
//
// The empty-bundle and blank-entry cases pin the other side: an operator who has declared nothing (or only a
// typo) leaves admission INERT, because arming strict enforcement off a whitespace entry would make every
// poll on that deployment unvotable.
func TestConfiguredIsABundlePropertyNotAnActionProperty(t *testing.T) {
	mk := func(t *testing.T, id string, approveBy []string) policy.Rule {
		t.Helper()
		r, err := policy.NewRule(policy.Rule{
			ID: id, Match: policy.Match{OpClass: id}, Verdict: policy.VerdictApprove, ApproveBy: approveBy,
		})
		if err != nil {
			t.Fatalf("build rule %s: %v", id, err)
		}
		return r
	}
	governed := mk(t, "restart-service", nil)                      // the action being voted on names NOBODY
	elsewhere := mk(t, "cfg.change", []string{"group:sre-oncall"}) // ...but the operator named approvers HERE
	blankOnly := mk(t, "disk.trim", []string{"   ", ""})           // a typo is not a declaration
	for _, tc := range []struct {
		name string
		set  policy.RuleSet
		want bool
	}{
		{"an empty bundle declares no approver regime", policy.RuleSet{}, false},
		{"a bundle whose only rule names nobody is unconfigured", policy.RuleSet{Rules: []policy.Rule{governed}}, false},
		{"blank entries are not a declaration", policy.RuleSet{Rules: []policy.Rule{governed, blankOnly}}, false},
		{"one rule declaring approvers configures the WHOLE bundle", policy.RuleSet{Rules: []policy.Rule{governed, elsewhere}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rulesDeclaringApprovers(tc.set) > 0; got != tc.want {
				t.Fatalf("configured = %v, want %v — this is what arms (or leaves inert) vote admission for every poll on the deployment", got, tc.want)
			}
		})
	}
	// And the pairing that matters: in the configured bundle, the action being gated still resolves an EMPTY
	// approver set. "Configured" and "this action has approvers" are genuinely different facts, and the
	// workflow needs BOTH to tell "nobody may approve this" from "nobody was ever asked".
	eng, err := policy.NewEngine(context.Background(), policy.RuleSet{Rules: []policy.Rule{governed, elsewhere}})
	if err != nil {
		t.Fatalf("build the engine: %v", err)
	}
	if got := approveByFor(context.Background(), eng, nil, func() policy.Mode { return policy.ModeShadow }, approveByProbe); len(got) != 0 {
		t.Fatalf("the gated action's own rule declares no approver, so its set must be empty even though the bundle is configured, got %v", got)
	}
}

// failingDecider is the identity/policy backend that cannot answer — it must DENY, never default open.
type failingDecider struct{}

func (failingDecider) Decide(context.Context, policy.EvalInput) (policy.PolicyDecision, error) {
	return policy.PolicyDecision{}, context.DeadlineExceeded
}

// mkRule builds a policy.Rule with the given approvers over a distinct op-class (so a RuleSet can hold several).
func mkRule(t *testing.T, id, opClass string, approveBy []string) policy.Rule {
	t.Helper()
	r, err := policy.NewRule(policy.Rule{ID: id, Match: policy.Match{OpClass: opClass}, Verdict: policy.VerdictApprove, ApproveBy: approveBy})
	if err != nil {
		t.Fatalf("build rule %s: %v", id, err)
	}
	return r
}

// TestMatrixApproversStranded is the TG-437 oracle: the PER-RULE boot cross-check that catches the disjoint
// approver-namespace defect. TG_MATRIX_APPROVERS holds Matrix MXIDs; the vote lane signals the raw MXID as the
// voter; runner.VoterAdmitted checks it PER-POLL against the one governing rule's approve_by. A Matrix
// identity absent from a rule is refused on that rule's polls — the exact live 2026-08-10 failure (@dominicus
// with approve_by=[user:kyriakos]), and, critically, its PARTIAL form (in some rules, not others) which a
// union check would miss.
//
// KILLING MUTATION: make matrixApproversStranded return nil unconditionally — arms (1) and (partial) go RED.
func TestMatrixApproversStranded(t *testing.T) {
	mx := "@dominicus:matrix.example.net"
	strandedOf := func(s []strandedMatrixApprover) []string {
		var out []string
		for _, x := range s {
			out = append(out, x.Approver)
		}
		return out
	}

	// (1) THE LIVE BUG (fully vacuous): approve_by names operator LOGIN identities; the MXID is in NO rule.
	disjoint := policy.RuleSet{Rules: []policy.Rule{mkRule(t, "r1", "restart-service", []string{"user:kyriakos", "user:kyriakosp"})}}
	got := matrixApproversStranded([]string{mx}, disjoint, nil, nil)
	if len(got) != 1 || got[0].Approver != mx || len(got[0].RefusedBy) != got[0].Total || got[0].Total != 1 {
		t.Fatalf("a Matrix approver admissible by NO rule must be reported FULLY stranded (RefusedBy==Total); got %+v", got)
	}

	// (2) FIXED (in every rule): silent.
	fixed := policy.RuleSet{Rules: []policy.Rule{mkRule(t, "r1", "restart-service", []string{"user:kyriakos", "user:" + mx})}}
	if got := matrixApproversStranded([]string{mx}, fixed, nil, nil); len(got) != 0 {
		t.Fatalf("a Matrix approver present in every rule's approve_by must NOT be stranded; got %+v", got)
	}

	// (3) admissible via a GROUP the human plane expands to include the MXID — no false alarm.
	viaGroup := policy.RuleSet{Rules: []policy.Rule{mkRule(t, "r1", "restart-service", []string{"group:matrix-approvers"})}}
	human := stubHumanPlane{members: map[string][]string{"matrix-approvers": {mx}}}
	if got := matrixApproversStranded([]string{mx}, viaGroup, human, nil); len(got) != 0 {
		t.Fatalf("a Matrix approver reachable through a group must not be stranded; got %+v", got)
	}

	// (4) THE PARTIAL CASE the union check MISSED (the review finding): admissible by r1, REFUSED by r2 —
	// r2's polls silently reject the approver. Must be reported, and NOT as fully vacuous.
	partial := policy.RuleSet{Rules: []policy.Rule{
		mkRule(t, "r1", "restart-service", []string{"user:" + mx}),
		mkRule(t, "r2", "reload-service", []string{"user:kyriakos"}),
	}}
	got = matrixApproversStranded([]string{mx}, partial, nil, nil)
	if len(got) != 1 || !slices.Equal(got[0].RefusedBy, []string{"r2"}) || got[0].Total != 2 {
		t.Fatalf("a Matrix approver refused by SOME rules must be reported partial, naming the refusing rule(s); got %+v", got)
	}

	// (5) rules that declare NO approver are skipped — they are not a namespace question (covered by the
	// unconfigured/resolvable states). A bundle where the ONLY approver-declaring rule admits the MXID is silent.
	withUnconfigured := policy.RuleSet{Rules: []policy.Rule{
		mkRule(t, "r1", "restart-service", []string{"user:" + mx}),
		mkRule(t, "r2", "reload-service", nil), // no approver regime
	}}
	if got := matrixApproversStranded([]string{mx}, withUnconfigured, nil, nil); len(got) != 0 {
		t.Fatalf("a rule declaring no approver must not count against a Matrix approver; got %+v", got)
	}

	// (6) mixed approvers: only the stranded one is returned.
	other := "@ghost:matrix.example.net"
	if got := matrixApproversStranded([]string{mx, other}, fixed, nil, nil); !slices.Equal(strandedOf(got), []string{other}) {
		t.Fatalf("only the stranded approver must be reported; got %+v", got)
	}

	// (7) empty-input arms (TG-365): no Matrix approvers ⇒ nothing; no approver-declaring rules ⇒ nothing
	// (there is no approver regime to be disjoint FROM).
	if got := matrixApproversStranded(nil, disjoint, nil, nil); len(got) != 0 {
		t.Fatalf("no Matrix approvers ⇒ nothing to strand; got %+v", got)
	}
	if got := matrixApproversStranded([]string{mx}, policy.RuleSet{}, nil, nil); len(got) != 0 {
		t.Fatalf("no approver-declaring rules ⇒ nothing to strand (no regime to mismatch); got %+v", got)
	}
}

// stubHumanPlane is the spec/016 credential human plane narrowed to the one question expandApproveBy asks:
// who is in this group? It stands in for a synced LDAP/FreeIPA source without standing one up.
type stubHumanPlane struct {
	members map[string][]string
	err     error
}

func (s stubHumanPlane) ResolveApprovers(q credential.ApproverQuery) (credential.ApproverSet, error) {
	if s.err != nil {
		return credential.ApproverSet{}, s.err
	}
	u, ok := s.members[q.Group]
	if !ok {
		// The human plane's fail-closed sentinel shape: a group it names no approver for is an error, never
		// an empty-but-successful set (core/credential/plane.go: "An empty set is never returned with a nil error").
		return credential.ApproverSet{}, errors.New("credential: no approver resolves for that group")
	}
	return credential.ApproverSet{Users: u}, nil
}

// TestAGroupOnlyApproveByIsUsableEndToEnd is the oracle for the gap that made this control INERT for the only
// spelling anyone writes. `group:sre-oncall` is the approve_by form in design.md, in core/policy's own pipeline
// guard rule, and in the console's rule renderer — and the vote admission runs in workflow code, which has no
// identity backend and so admits NOBODY for a bare group entry.
//
// So it asserts the FULL shipped chain: the policy engine's rule → approveByFor (what main() installs) → the
// EXACT predicate the workflow enforces (runner.VoterAdmitted, not a re-typed copy). A member of the group is
// admitted; a stranger is not.
func TestAGroupOnlyApproveByIsUsableEndToEnd(t *testing.T) {
	human := stubHumanPlane{members: map[string][]string{"sre-oncall": {"alice", "bob"}}}
	got := approveByFor(context.Background(), approveByEngine(t, []string{"group:sre-oncall"}),
		human, func() policy.Mode { return policy.ModeShadow }, approveByProbe)

	if !runner.VoterAdmitted(got, "alice") {
		t.Fatalf("a member of the approve_by GROUP must be admitted by the rule the workflow enforces — otherwise the documented config form yields a poll nobody can approve; expanded set was %v", got)
	}
	if !runner.VoterAdmitted(got, "bob") {
		t.Fatalf("every member of the group must be admitted, not just the first; expanded set was %v", got)
	}
	if runner.VoterAdmitted(got, "mallory") {
		t.Fatalf("expansion must not admit a non-member: %v", got)
	}
}

// TestAGroupApproveByWithoutAHumanPlaneAdmitsNobody is the direction control. When there is no identity
// backend to expand against, the group MUST resolve to a set that admits nobody — never to "anyone". This is
// the state the boot log has to shout about, and it is what makes the expansion above a real capability rather
// than a widening of the check.
func TestAGroupApproveByWithoutAHumanPlaneAdmitsNobody(t *testing.T) {
	for _, tc := range []struct {
		name  string
		human approverExpander
	}{
		{"no human plane wired", nil},
		{"the plane names no member for the group", stubHumanPlane{members: map[string][]string{}}},
		{"the plane cannot answer", stubHumanPlane{err: errors.New("ldap unreachable")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := approveByFor(context.Background(), approveByEngine(t, []string{"group:sre-oncall"}),
				tc.human, func() policy.Mode { return policy.ModeShadow }, approveByProbe)
			if runner.VoterAdmitted(got, "alice") || runner.VoterAdmitted(got, "mallory") {
				t.Fatalf("an unexpandable group must admit NOBODY (fail closed): %v", got)
			}
			// ...and the boot log must be able to SAY so, rather than counting the rule as healthy because it
			// declared an approver. This is the check that distinguishes "declared" from "usable".
			if approveByNamesAConcretePrincipal(got) {
				t.Fatalf("an unexpandable group must not read as naming a concrete principal: %v", got)
			}
		})
	}
}

// TestExpandApproveByKeepsUserEntriesWhenAGroupFails proves a group-resolution failure cannot cost the poll the
// approvers that WERE named directly — the failure is contained to the group it belongs to.
func TestExpandApproveByKeepsUserEntriesWhenAGroupFails(t *testing.T) {
	got := expandApproveBy([]string{"group:sre-oncall", "user:kyriakos"}, stubHumanPlane{err: errors.New("ldap unreachable")})
	if !runner.VoterAdmitted(got, "kyriakos") {
		t.Fatalf("a directly-named approver must survive a group-resolution failure: %v", got)
	}
	if !approveByNamesAConcretePrincipal(got) {
		t.Fatalf("a set containing a user: entry names a concrete principal: %v", got)
	}
}

// TestExpandApproveByIsDeterministic locks the property Temporal actually requires of this value: it is
// RECORDED IN WORKFLOW HISTORY, so two resolutions of the same rule must produce the byte-identical set or a
// replay diverges. Group members arrive unordered from the identity backend; sorting is not cosmetic here.
func TestExpandApproveByIsDeterministic(t *testing.T) {
	a := expandApproveBy([]string{"group:sre-oncall"}, stubHumanPlane{members: map[string][]string{"sre-oncall": {"bob", "alice", "carol"}}})
	b := expandApproveBy([]string{"group:sre-oncall"}, stubHumanPlane{members: map[string][]string{"sre-oncall": {"carol", "alice", "bob"}}})
	if !slices.Equal(a, b) {
		t.Fatalf("the recorded approver set must not depend on the identity backend's ordering: %v vs %v", a, b)
	}
	// And a duplicate (named both directly and via the group) appears once — a set, not a bag.
	dup := expandApproveBy([]string{"user:alice", "group:sre-oncall"}, stubHumanPlane{members: map[string][]string{"sre-oncall": {"alice"}}})
	if len(dup) != 2 { // "user:alice" + "group:sre-oncall"
		t.Fatalf("a principal named twice must appear once: %v", dup)
	}
}

// TG-437 SECOND HALF — the boot check must ask about the identity the VOTE PATH presents, not the one
// the notifier config spells. The inbound lane normalizes a chat identity through TG-463's alias map
// before admission; this check used to compare the RAW MXID, so on a deployment WITH an alias the two
// halves disagreed by construction: votes were admitted correctly while the boot log announced that
// every one of them was refused, and advised adding the raw MXID to approve_by — unnecessary, and not
// what admission compares. A correct posture reported as a fault, with a remedy that weakens it.
//
// KILLING MUTATION: drop the `presented := normalize(m)` line and check `m` again — case (1) below goes
// RED with a stranded report for an approver whose votes are in fact admitted.
func TestMatrixStrandCheckAsksAboutTheIdentityTheVotePathPresents(t *testing.T) {
	const mx = "@dominicus:matrix.example.net"
	const login = "kyriakos"
	alias := func(v string) string {
		if v == mx {
			return login
		}
		return v
	}
	// (1) The alias maps the MXID onto a login approve_by DOES name: nothing is stranded, and the boot
	//     log must stay silent rather than page about a working vote path.
	works := policy.RuleSet{Rules: []policy.Rule{mkRule(t, "r1", "restart-service", []string{"user:" + login})}}
	if got := matrixApproversStranded([]string{mx}, works, nil, alias); len(got) != 0 {
		t.Fatalf("an aliased approver whose canonical login IS in approve_by must NOT be reported stranded — "+
			"the vote path admits it; got %+v", got)
	}
	// (2) The alias maps it onto a login approve_by does NOT name: still stranded, and the ADVICE must
	//     name the presented login (adding the raw MXID would not fix the refusal).
	broken := policy.RuleSet{Rules: []policy.Rule{mkRule(t, "r1", "restart-service", []string{"user:someone-else"})}}
	got := matrixApproversStranded([]string{mx}, broken, nil, alias)
	if len(got) != 1 {
		t.Fatalf("an aliased approver whose canonical login is absent must still be stranded; got %+v", got)
	}
	if got[0].Approver != mx {
		t.Errorf("the report must name the CONFIGURED identity the operator edits, got %q", got[0].Approver)
	}
	if got[0].Presented != login {
		t.Errorf("the report must carry the PRESENTED identity (%q) so the advice names what approve_by must "+
			"contain, got %q", login, got[0].Presented)
	}
	if adv := strandedAdviceIdentity(got[0]); adv != login {
		t.Errorf("the boot advice must tell the operator to add %q (what admission compares), got %q", login, adv)
	}
	// (3) No alias in play: the advice names the configured identity, exactly as before this change.
	plain := matrixApproversStranded([]string{mx}, broken, nil, nil)
	if len(plain) != 1 || plain[0].Presented != "" || strandedAdviceIdentity(plain[0]) != mx {
		t.Fatalf("with no alias the behaviour must be unchanged (advice names the MXID); got %+v", plain)
	}
}
