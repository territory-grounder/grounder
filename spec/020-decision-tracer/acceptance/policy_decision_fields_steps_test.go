package acceptance

import (
	"context"
	"fmt"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
)

// T-020-2 binds REQ-2004: a persisted policy_decision row carries the in-force bundle_version AND the FULL
// matched-rules list (REQ-1522 deny-overrides provenance) — so the tracer can show WHICH rule set decided and
// WHICH operator rules matched, instead of a bare verdict. The oracle drives the REAL AuditedEngine.Decide over
// a real Engine loaded with a one-rule operator bundle whose rule MATCHES the action, into db.MemPolicyDecisionStore
// (the pgx AuditSink twin the CI oracles use without a database). It then reads the persisted-twin record back and
// asserts the bundle_version equals the engine's in-force version (non-empty) and the matched-rules list is carried
// with the matched rule present — proving neither is dropped at the audit projection. The pgx INSERT/SELECT of
// bundle_version + matched_rules is proven separately by the DSN-gated core/db policy-store round-trip
// (TestPolicyStoresRoundTrip). Observe-only: recording these provenance fields NEVER changes a verdict — Decide
// composes identically whether or not the audit sink is present.
func init() {
	stepRegistrars = append(stepRegistrars, registerPolicyDecisionFieldsSteps)
}

type policyDecisionFieldsWorld struct {
	eng  *policy.Engine
	in   policy.EvalInput
	dec  policy.PolicyDecision
	sink *db.MemPolicyDecisionStore
}

func registerPolicyDecisionFieldsSteps(sc *godog.ScenarioContext) {
	w := &policyDecisionFieldsWorld{}

	sc.Step(`^a policy evaluation that computes a bundle_version a matched-rules list and a reason in memory$`, func() error {
		// A one-rule operator bundle whose rule matches the action's raw command (the deny side of a match). A
		// non-empty rule set makes the engine derive a non-empty content-addressed bundle_version, and a matching
		// rule makes the evaluation yield a non-empty matched-rules list (deny-overrides provenance).
		rule, err := policy.NewRule(policy.Rule{ID: "no-rm-rf", Match: policy.Match{ArgvPattern: "rm -rf"}, Verdict: policy.VerdictDeny})
		if err != nil {
			return err
		}
		eng, err := policy.NewEngine(context.Background(), policy.RuleSet{Rules: []policy.Rule{rule}})
		if err != nil {
			return err
		}
		w.eng = eng
		w.in = policy.EvalInput{
			OpClass: "shell-exec",
			Argv:    "rm -rf /tmp/x", // matches the rule's ArgvPattern → the rule matches
			Host:    "librespeed01",
			Band:    safety.BandAuto,
			Mode:    policy.ModeShadow,
		}
		return nil
	})

	sc.Step(`^the policy-decision row is written and read back$`, func() error {
		w.sink = db.NewMemPolicyDecisionStore()
		ae := policy.NewAuditedEngine(w.eng, w.sink)
		dec, err := ae.Decide(context.Background(), w.in)
		if err != nil {
			return err
		}
		w.dec = dec
		return nil
	})

	sc.Step(`^the row carries a bundle_version equal to the in-memory value and a matched_rules list whose length equals the in-memory matched-rules length and the decision reason$`, func() error {
		if w.sink == nil || len(w.sink.Records) != 1 {
			return fmt.Errorf("want exactly 1 audited decision, got %d", len(w.sink.Records))
		}
		row := w.sink.Records[0]

		// bundle_version equals the engine's in-force version AND is non-empty — a decision must name which rule
		// set produced it, or the tracer cannot bind the verdict back to a bundle (REQ-1522).
		if bv := row.BundleVersion(); bv == "" || bv != w.eng.BundleVersion() {
			return fmt.Errorf("bundle_version: got %q, want the engine's in-force %q (non-empty)", bv, w.eng.BundleVersion())
		}

		// matched_rules length equals the in-memory decision's — carried through the audit projection, not dropped.
		// The evaluation matched exactly the one loaded rule, so the in-memory length is non-zero and known.
		wantLen := len(w.dec.MatchedRules())
		if wantLen == 0 {
			return fmt.Errorf("in-memory matched-rules is empty — the rule did not match, so the oracle proves nothing")
		}
		if got := len(row.MatchedRules()); got != wantLen {
			return fmt.Errorf("matched_rules length: got %d, want the in-memory %d — the provenance list was dropped at the audit projection", got, wantLen)
		}
		if row.MatchedRules()[0].ID != "no-rm-rf" {
			return fmt.Errorf("matched_rules[0].id: got %q, want %q — the matched rule identity was not carried", row.MatchedRules()[0].ID, "no-rm-rf")
		}

		// the decision reason (the packet-tracer explanation) is present.
		if row.Reason() == "" {
			return fmt.Errorf("decision reason empty — the human-readable explanation was dropped")
		}
		return nil
	})
}
