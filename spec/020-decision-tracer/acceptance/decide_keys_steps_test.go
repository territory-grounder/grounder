package acceptance

import (
	"context"
	"fmt"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
)

// T-020-3 binds REQ-2005: the audited policy_decision carries the action_id, external_ref, and principal so it
// joins the decision-tracer walk by BOTH correlation keys instead of the empty columns migration 0019 left. The
// oracle drives the REAL AuditedEngine.Decide over a real Engine into db.MemPolicyDecisionStore (the pgx
// AuditSink twin), so the audit sink receives the exact PolicyDecision the pgx writer persists — proving the
// keys survive the audit projection without a database. The pgx INSERT of the new external_ref column is proven
// by the DSN-gated core/db round-trip. Observe-only: threading these keys NEVER changes a verdict (Decide
// composes identically with them empty).
func init() {
	stepRegistrars = append(stepRegistrars, registerDecideKeysSteps)
}

type decideKeysWorld struct {
	in   policy.EvalInput
	sink *db.MemPolicyDecisionStore
}

func registerDecideKeysSteps(sc *godog.ScenarioContext) {
	w := &decideKeysWorld{}

	sc.Step(`^a policy Decide evaluation on a classified action$`, func() error {
		// The interceptor threads the sealed manifest's content-hashed action_id (INV-07), the incident it
		// answers, and the acting runner principal into EvalInput (never a secret; audit-only).
		w.in = policy.EvalInput{
			OpClass:     "restart-service",
			Host:        "librespeed01",
			Band:        safety.BandAuto,
			Mode:        policy.ModeShadow,
			ActionID:    "act-req2005",
			ExternalRef: "ext-req2005",
			Principal:   "runner:ext-req2005",
		}
		return nil
	})

	sc.Step(`^the audited engine writes its per-evaluation row$`, func() error {
		eng, err := policy.NewEngine(context.Background(), policy.RuleSet{})
		if err != nil {
			return err
		}
		w.sink = db.NewMemPolicyDecisionStore()
		ae := policy.NewAuditedEngine(eng, w.sink)
		if _, err := ae.Decide(context.Background(), w.in); err != nil {
			return err
		}
		return nil
	})

	sc.Step(`^the row carries the action_id the external_ref and the principal so the policy record joins into the walk by both correlation keys rather than empty key columns$`, func() error {
		if w.sink == nil || len(w.sink.Records) != 1 {
			return fmt.Errorf("want exactly 1 audited decision, got %d", len(w.sink.Records))
		}
		d := w.sink.Records[0]
		if d.ActionID() != w.in.ActionID {
			return fmt.Errorf("action_id: got %q, want %q", d.ActionID(), w.in.ActionID)
		}
		if d.ExternalRef() != w.in.ExternalRef {
			return fmt.Errorf("external_ref: got %q, want %q — the correlation key was dropped at the audit projection", d.ExternalRef(), w.in.ExternalRef)
		}
		if d.Principal() != w.in.Principal {
			return fmt.Errorf("principal: got %q, want %q", d.Principal(), w.in.Principal)
		}
		// REQ-2005: "rather than empty key columns" — both correlation keys must be NON-EMPTY so the record joins
		// the walk instead of orphaning on empty columns.
		if d.ActionID() == "" || d.ExternalRef() == "" {
			return fmt.Errorf("correlation keys empty — record would not join the walk: action_id=%q external_ref=%q", d.ActionID(), d.ExternalRef())
		}
		return nil
	})
}
