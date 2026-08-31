package main

import (
	"context"
	"log"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// logMatrixApproverStranding runs the TG-437 namespace cross-check on a ruleset and logs any stranded Matrix
// approver, tagged by `where` (e.g. "boot" or "ruleset-write"). It is shared by the boot check AND the
// ruleset-write activity (via rulesetwrite.Deps.OnParsed): a runtime ruleset write that re-strands the
// Matrix approver — the exact regression that reopened TG-437 — now surfaces IMMEDIATELY at write time,
// instead of silently until the next boot re-runs the boot check. Aliases are the TG-463 voter-alias map so
// the check asks about the identity the vote lane will actually present. A no-op when no Matrix approver is
// configured.
func logMatrixApproverStranding(matrixApprovers []string, aliases map[string]string, rs policy.RuleSet, human approverExpander, where string) {
	if len(matrixApprovers) == 0 {
		return
	}
	for _, st := range matrixApproversStranded(matrixApprovers, rs, human, func(v string) string { return normalizeVoter(aliases, v) }) {
		if len(st.RefusedBy) == st.Total {
			log.Printf("policy: MATRIX APPROVAL VACUOUS (TG-437, %s) — Matrix approver %q is admissible by NONE of the %d approver-declaring rule(s), so EVERY Matrix vote from it is refused (human:vote-refused:not-in-approve-by). TG_MATRIX_APPROVERS (a Matrix MXID) and the ruleset approve_by are disjoint identity namespaces and nothing else cross-checks them. FIX: add it to approve_by as a user: entry (user:%s), or leave the Matrix notifier unconfigured.",
				where, st.Approver, st.Total, strandedAdviceIdentity(st))
		} else {
			log.Printf("policy: MATRIX APPROVER PARTIALLY STRANDED (TG-437, %s) — Matrix approver %q is refused by %d of %d approver-declaring rule(s) %v: its votes on those rules' polls are silently rejected (human:vote-refused:not-in-approve-by). If that per-rule scoping is deliberate, ignore this; otherwise add user:%s to those rules' approve_by.",
				where, st.Approver, len(st.RefusedBy), st.Total, st.RefusedBy, strandedAdviceIdentity(st))
		}
	}
}

// ---------------------------------------------------------------------------------------------------------
// THE approve_by TRANSLATION (spec/015 REQ-1516, TG-254).
//
// temporal/runner deliberately never imports core/policy, so the runner asks "who may approve this poll?" as
// a plain-value seam (runner.Deps.ApproveByFor) and THIS composition root answers it over the one real policy
// engine — exactly the shape the LadderRung translation next door uses.
//
// It is a NAMED function, not an inline closure in the Deps literal, for the reason ladderRungFor documents:
// an aliveness oracle must be able to drive the SAME code the shipped binary wires. A rule table re-typed
// inside a test proves the test's copy is right and says nothing about the binary.
//
// WHY THIS EXISTS AT ALL: until it did, `core/policy.MayApprove` / `VoteAdmission.Admit` had ZERO production
// callers. /v1/vote authenticated the operator, rate-limited them, and signalled the workflow — and no
// approver check ran anywhere in the product. Any authenticated operator could approve any governed action.
// ---------------------------------------------------------------------------------------------------------

// approveByDecider is the narrow read this file needs from the policy engine. Narrowed (rather than taking
// *policy.Engine) so the wiring oracle can drive a stub, and so it is obvious that nothing here can mutate
// policy state — resolving approve_by is a READ of the rule that governs an action, never a decision.
type approveByDecider interface {
	Decide(ctx context.Context, in policy.EvalInput) (policy.PolicyDecision, error)
}

// approverExpander is the narrow read this file needs from the spec/016 credential HUMAN plane: given a group
// / role name, WHO are its members? *credential.SyncEngine satisfies it (ResolveApprovers), and it is narrowed
// so the oracle can drive a stub without standing up a whole synced identity backend.
type approverExpander interface {
	ResolveApprovers(q credential.ApproverQuery) (credential.ApproverSet, error)
}

// expandApproveBy turns each `group:` entry of a rule's approve_by into the CONCRETE `user:` principals that
// group contains, resolved HERE (at gate time, in the composition root) rather than at signal time.
//
// ★ WITHOUT THIS THE WHOLE CONTROL IS INERT FOR THE ONLY CONFIG FORM ANYONE WRITES. The vote admission runs in
// workflow code, which deliberately has no identity backend — a live membership lookup there would resolve
// differently on replay after a group edit and break determinism, so a bare `group:` entry admits NOBODY once
// it crosses the seam. Every approve_by example in this repo is a group (`group:sre-oncall` in the policy
// pipeline guard, in design.md, in the console's rule renderer). So an owner who declares
// `approve_by: ["group:sre-oncall"]` — the documented spelling — would get a poll that is fail-closed
// UNVOTABLE, while the boot log cheerfully reported "1 rule declares approvers". Silent, and indistinguishable
// from the deployment working.
//
// The expansion is FROZEN at gate time by construction: the members are recorded in the workflow's history, so
// someone who joins the on-call group after the poll opened cannot approve it. That is the price of
// deterministic replay and it is the safe direction — the alternative (re-resolving membership at signal time)
// would let a group edit retroactively change who could approve a poll that is already parked.
//
// FAIL CLOSED at every step: no human plane, a group the plane names no member for (the ErrUnresolved
// sentinel), or a backend error all contribute NOTHING. The original entries are kept verbatim so the recorded
// set still says which rule text governed, and so a `user:` entry is never lost to a group failure.
//
// Only ONE level is expanded: an ApproverSet that names further GROUPS is not recursed into, because the human
// plane offers no cycle-safe closure and a fail-closed non-expansion is the correct direction for a control
// whose error mode must be "a real approver retries", never "a stranger executes".
func expandApproveBy(entries []string, human approverExpander) []string {
	out := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	add := func(e string) {
		e = strings.TrimSpace(e)
		if e == "" {
			return
		}
		k := strings.ToLower(e)
		if _, dup := seen[k]; dup {
			return
		}
		seen[k] = struct{}{}
		out = append(out, e)
	}
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		add(e)
		if human == nil || !strings.HasPrefix(e, "group:") {
			continue
		}
		group := strings.TrimSpace(strings.TrimPrefix(e, "group:"))
		if group == "" {
			continue
		}
		set, err := human.ResolveApprovers(credential.ApproverQuery{Group: group})
		if err != nil {
			// Including credential.ErrUnresolved — "this group names no approver" is the human plane's
			// fail-closed sentinel, not a hard failure. Either way the group contributes no member.
			continue
		}
		members := append([]string(nil), set.Users...)
		sort.Strings(members) // deterministic: this set is recorded in workflow history and must replay identically
		for _, u := range members {
			add("user:" + u)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// rulesDeclaringApprovers counts the rules in a bundle that declare an `approve_by` at all — the BUNDLE-level
// fact that decides whether vote admission ENFORCES or is INERT (runner.Deps.ApproveByConfigured, TG-254).
//
// ★ IT IS A PROPERTY OF THE BUNDLE, NOT OF AN ACTION, and that is the whole point. Once ANY rule names an
// approver, the operator has expressed an approver regime, so an action whose own matched rule names nobody
// is one NOBODY may approve — the safe reading, and the one REQ-1516 asks for. Until then the operator has
// expressed no opinion at all, and enforcing an emptiness they never authored would refuse every vote on
// every poll (approve and deny alike) until each session timed out — turning "any operator can approve
// anything" into "no operator can approve anything, invisibly" on a deployment that actuates.
//
// A blank/whitespace-only entry does not count: it names no principal, so a rule carrying only blanks has
// declared nothing, and treating it as a declaration would arm strict enforcement off a typo.
func rulesDeclaringApprovers(rs policy.RuleSet) int {
	n := 0
	for _, r := range rs.Rules {
		for _, e := range r.ApproveBy {
			if strings.TrimSpace(e) != "" {
				n++
				break
			}
		}
	}
	return n
}

// approveByNamesAConcretePrincipal reports whether an (already expanded) approve_by set contains an entry the
// vote admission can actually match — i.e. anything that is not a bare, unexpanded `group:`. It exists ONLY to
// make the boot log honest: a bundle whose every approver is a group the human plane cannot expand is exactly
// as unvotable as one that declares no approver at all, and must say so just as loudly.
func approveByNamesAConcretePrincipal(entries []string) bool {
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" || strings.HasPrefix(e, "group:") {
			continue
		}
		if strings.HasPrefix(e, "user:") && strings.TrimSpace(strings.TrimPrefix(e, "user:")) == "" {
			continue
		}
		return true
	}
	return false
}

// strandedMatrixApprover names a configured Matrix approver together with the approver-declaring rules whose
// approve_by would REFUSE its vote. Empty RefusedBy is never returned (nothing to report).
type strandedMatrixApprover struct {
	Approver string
	// Presented is the identity the VOTE PATH will actually offer for admission when TG-463's alias map
	// rewrites it — empty when the alias map leaves the configured identity unchanged. It exists so the
	// boot advice can name BOTH: the string the operator edits and the string approve_by must contain.
	Presented string
	RefusedBy []string // rule IDs (in ruleset order) whose polls this Matrix approver cannot vote on
	Total     int      // how many rules declare an approver regime at all — RefusedBy==Total ⇒ fully vacuous
}

// matrixApproversStranded reports the TG-437 disjoint-namespace defect PER RULE. `TG_MATRIX_APPROVERS` holds
// Matrix MXIDs (e.g. `@dominicus:matrix.example.net`); a rule's `approve_by` may hold only operator
// login names (`user:kyriakos`). The vote lane signals the RAW MXID as the voter and the workflow admits it
// per-poll via runner.VoterAdmitted against the ONE governing rule's expanded approve_by — so a Matrix
// identity absent from a rule is refused on that rule's polls while the boot log's "N resolve to a concrete
// principal" certifies a DIFFERENT identity space. It happened live 2026-08-10: the sole Matrix approver was
// refused twice and nothing said so.
//
// It is PER-RULE, not the union, on purpose: the earlier union form missed the partial case (admissible by
// rule A, refused by rule B — B's polls still silently refuse the approver). For each Matrix approver this
// asks runner.VoterAdmitted (the SAME predicate the workflow runs) against EACH approver-declaring rule's
// expanded set, and reports every rule that refuses it. A rule that declares NO approver is skipped (its
// polls are governed by the unconfigured/resolvable states the caller already reports) — only rules that
// have expressed an approver regime yet exclude this Matrix identity are a namespace mismatch. RefusedBy ==
// Total is the fully-vacuous shape (the live bug); RefusedBy < Total is a partial vacuum that may also be a
// deliberate per-rule scoping, which the boot message says out loud so the operator can judge. Empty inputs
// (no Matrix approvers, or no approver-declaring rules) return nothing.
// matrixApproversStranded reports which configured Matrix approvers no approver-declaring rule would
// admit. normalize maps a presented chat identity to its canonical approve_by login (TG-463's alias
// map); pass nil for identity.
//
// ★ NORMALIZE FIRST, AND WHY THIS IS THE FIX FOR TG-437's SECOND HALF. The VOTE LANE normalizes before
// admission (cmd/worker/main.go wires normalizeVoter into the inbound lane), so the identity actually
// checked against approve_by at vote time is the CANONICAL login. This boot check used to compare the
// RAW MXID — so on a deployment that HAS an alias covering it, the two halves disagreed by construction:
// votes were admitted correctly while the boot log announced "EVERY Matrix vote from it is refused" and
// advised adding the raw MXID to approve_by, which is both unnecessary and wrong. That is the TG-344
// shape this repo has hit before — a correct posture reported as a fault, with a remedy that weakens it.
// Checking the SAME identity the vote path checks is what makes the warning mean what it says.
func matrixApproversStranded(matrixApprovers []string, rs policy.RuleSet, human approverExpander, normalize func(string) string) []strandedMatrixApprover {
	if normalize == nil {
		normalize = func(s string) string { return s }
	}
	// The approver-declaring rules, with their expanded sets computed once.
	type expandedRule struct {
		id  string
		set []string
	}
	var declaring []expandedRule
	for _, r := range rs.Rules {
		exp := expandApproveBy(r.ApproveBy, human)
		if len(exp) == 0 {
			continue // no approver regime on this rule — not a namespace question
		}
		declaring = append(declaring, expandedRule{id: r.ID, set: exp})
	}
	if len(declaring) == 0 {
		return nil
	}
	var out []strandedMatrixApprover
	for _, m := range matrixApprovers {
		if strings.TrimSpace(m) == "" {
			continue
		}
		// The identity the VOTE PATH will present, not the one the notifier config spells.
		presented := normalize(m)
		var refused []string
		for _, dr := range declaring {
			if !runner.VoterAdmitted(dr.set, presented) {
				refused = append(refused, dr.id)
			}
		}
		if len(refused) > 0 {
			// Report the CONFIGURED identity (what the operator would edit) and, when the alias changed
			// it, the one actually checked — otherwise the advice names a string that is not what the
			// vote path presents.
			st := strandedMatrixApprover{Approver: m, RefusedBy: refused, Total: len(declaring)}
			if presented != m {
				st.Presented = presented
			}
			out = append(out, st)
		}
	}
	return out
}

// approveByFor resolves the principals permitted to cast the approving vote on a gated POLL_PAUSE action:
// the `approve_by` list of the policy rule that governs it (Rule.ApproveBy, grammar `{user:* | group:*}`).
//
// It is asked at GATE time, once, and its answer is recorded in the workflow's history — so the later vote
// admission replays deterministically even if an operator edits the ruleset while the poll is parked.
//
// FAIL CLOSED, in the direction REQ-1516 requires: an unwired engine, a Decide error, a `deny` verdict (which
// short-circuits and carries no approvers — a refused action must not become approvable), or a matched rule
// that simply declares no approve_by ALL yield an EMPTY set, and an empty set admits NOBODY. The opposite
// default — "no approvers configured means anyone may approve" — is precisely the TG-254 defect.
//
// The UNAUDITED engine is used on purpose. This is a question about a rule, not an authorization of an
// action; routing it through the AuditedEngine would append a second policy_decision row per gated action and
// make the decision log (and the spec/020 tracer that joins on action_id) double-count every proposal.
//
// The matched rule's `group:` entries are EXPANDED to their members here — see expandApproveBy for why doing
// it anywhere else leaves the control inert for the only spelling anyone writes.
func approveByFor(ctx context.Context, eng approveByDecider, human approverExpander, modeNow func() policy.Mode, q runner.ApproveByQuery) []string {
	if eng == nil {
		return nil // no policy engine ⇒ no declared approvers ⇒ nobody may approve (fail closed)
	}
	mode := policy.ModeShadow // the zero/unknown mode is Shadow, matching the actuation chokepoint
	if modeNow != nil {
		mode = modeNow()
	}
	dec, err := eng.Decide(ctx, policy.EvalInput{
		OpClass:    q.OpClass,
		Host:       q.Host,
		Reversible: q.Reversible,
		// The FRESH per-incident band (TG-126), never a frozen first-seal band: band composition is what turns
		// a POLL_PAUSE action into the `approve` verdict that carries approve_by at all.
		Band: q.Band,
		Mode: mode,
		// Argv is deliberately EMPTY: the sealed argv does not exist until execute time (it is built from the
		// manifest at the effect leaf), so an argv-matched DENY rule cannot be evaluated here. That costs
		// nothing in safety — the interceptor re-Decides with the real argv before anything runs, and a deny
		// there still refuses. It only means this read never *widens* an approver set.
		// The non-secret correlation keys (spec/020 REQ-2005); they compose no verdict.
		ActionID:    q.ActionID,
		ExternalRef: q.ExternalRef,
		Principal:   "gate:" + q.ExternalRef,
	})
	if err != nil {
		return nil // an engine that cannot answer names no approver (fail closed)
	}
	return expandApproveBy(dec.ApproveBy(), human)
}

// strandedAdviceIdentity is the identity a boot warning should tell the operator to ADD to approve_by:
// the one the vote path will present. When TG-463's alias map rewrites the configured Matrix MXID, the
// canonical login is what admission compares — naming the raw MXID there would be advice that does not
// fix the refusal.
func strandedAdviceIdentity(st strandedMatrixApprover) string {
	if strings.TrimSpace(st.Presented) != "" {
		return st.Presented
	}
	return st.Approver
}
