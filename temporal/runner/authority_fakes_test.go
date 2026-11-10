package runner

import (
	"context"

	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/verify"
	"github.com/territory-grounder/grounder/temporal/runner/runnertest"
	"github.com/territory-grounder/grounder/temporal/wfauth/wfauthtest"
)

// permissiveAuthority wires the LOOSEST durable records and an admit-everything envelope verifier into d — only
// where d has none — so an oracle whose subject is something else (the interceptor chain, a ledger ordering, a
// vote state machine) keeps the semantics it had before TG-660: every POLL_PAUSE action has an approval row, the
// classifier recorded the loosest band, every vote is signed. The PREDICTION has no permissive form — the row is
// bound to the session/action/target/site — so the reader is the gate's own store (the record GateActivity
// committed is the one ExecuteActivity reads back, as in production), or an empty store when the rig has no
// gate. The refusals themselves are proven by execute_authority_test.go and vote_auth_test.go with STRICT fakes.
func permissiveAuthority(d *Deps) {
	if d.Predictions == nil {
		if d.Gate != nil && d.Gate.Store != nil {
			d.Predictions = d.Gate.Store
		} else {
			d.Predictions = &runnertest.Predictions{}
		}
	}
	if d.Approvals == nil {
		d.Approvals = &runnertest.Approvals{ApproveAll: true}
	}
	if d.RiskAudits == nil {
		d.RiskAudits = &runnertest.RiskAudits{}
	}
	if d.Executions == nil {
		d.Executions = &runnertest.Executions{}
	}
	if d.PlanSteps == nil {
		d.PlanSteps = &runnertest.PlanSteps{}
	}
	if d.WorkflowAuth == nil {
		d.WorkflowAuth = wfauthtest.AllowAll{}
	}
}

// permissive is permissiveAuthority for an inline Deps literal: NewActivities(permissive(Deps{...})).
func permissive(d Deps) Deps {
	permissiveAuthority(&d)
	return d
}

// predicted commits, into d's prediction reader, the record the gate would have committed for in — the session,
// action, plan hash, target and site the input names — and returns in. For the oracles that call ExecuteActivity
// directly (no GateActivity ran): ExecuteActivity binds the row to the input, so a wildcard cannot stand in.
func predicted(d Deps, in ExecuteInput) ExecuteInput {
	rec := predict.PredictionRecord{
		Prediction:  verify.Prediction{ActionID: in.ActionID, PlanHash: in.PlanHash, TargetHost: in.TargetHost, Site: in.Site},
		ExternalRef: in.ExternalRef,
	}
	switch p := d.Predictions.(type) {
	case *runnertest.Predictions:
		p.Commit(in.PlanHash, rec)
	case predict.PredictionStore:
		_ = p.Commit(context.Background(), rec)
	}
	return in
}
