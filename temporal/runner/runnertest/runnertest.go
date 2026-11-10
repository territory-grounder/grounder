// Package runnertest is TEST SUPPORT for the Runner's durable-authority seams (TG-660 part c): in-memory
// PredictionReader / ApprovalReader / RiskAuditReader fakes for oracles whose subject is not the authority
// re-derivation itself (the acceptance suites, the interceptor-chain proofs). It deliberately does NOT import the
// runner package (the runner's own tests import it), so wiring is the caller's three lines. It is
// imported only by _test.go files — never by a binary — so no composition root can wire a permissive record
// store by accident, and the deadcode gate (rooted at the binaries) never sees it.
package runnertest

import (
	"context"
	"sync"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// Predictions is an in-memory runner.PredictionReader: only hashes set through Commit (or For) are known. There
// is deliberately NO wildcard mode — ExecuteActivity binds the row to the input's session, action, target and
// site, so a rig that executes commits the record the gate would have committed (For), exactly as the workflow
// rigs do through the gate's own MemPredictionStore.
type Predictions struct {
	mu   sync.Mutex
	rows map[string]predict.PredictionRecord
}

// Commit records a prediction under planHash.
func (p *Predictions) Commit(planHash string, rec predict.PredictionRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rows == nil {
		p.rows = map[string]predict.PredictionRecord{}
	}
	p.rows[planHash] = rec
}

// For commits the record the prediction gate commits for one gated action: the row names the session, the
// action, its plan hash, target and site.
func (p *Predictions) For(externalRef, actionID, planHash, target, site string) {
	p.Commit(planHash, predict.PredictionRecord{
		Prediction:  verify.Prediction{ActionID: actionID, PlanHash: planHash, TargetHost: target, Site: site},
		ExternalRef: externalRef,
	})
}

// Get implements runner.PredictionReader.
func (p *Predictions) Get(_ context.Context, planHash string) (predict.PredictionRecord, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rec, ok := p.rows[planHash]; ok {
		return rec, true, nil
	}
	return predict.PredictionRecord{}, false, nil
}

// Approvals is an in-memory runner.ApprovalReader keyed on (actionID, externalRef). ApproveAll=true answers
// every lookup with a human:approve row.
type Approvals struct {
	ApproveAll bool
	mu         sync.Mutex
	rows       map[string]string
}

func approvalKey(actionID, externalRef string) string { return actionID + "\x00" + externalRef }

// Record stores an approval decision ("human:approve" or "fallback:autonomous-approve:<key>") for the pair.
func (a *Approvals) Record(actionID, externalRef, decision string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rows == nil {
		a.rows = map[string]string{}
	}
	a.rows[approvalKey(actionID, externalRef)] = decision
}

// ApprovalFor implements runner.ApprovalReader.
func (a *Approvals) ApprovalFor(_ context.Context, actionID, externalRef string) (string, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if d, ok := a.rows[approvalKey(actionID, externalRef)]; ok {
		return d, true, nil
	}
	if a.ApproveAll {
		return "human:approve", true, nil
	}
	return "", false, nil
}

// RiskAudits is an in-memory runner.RiskAuditReader. Default answers (BandAuto, "", ok=true) for every session —
// the loosest record, so the executed band is whatever the input claims (the pre-TG-660 behaviour); Set pins a
// specific record for a session.
type RiskAudits struct {
	// Absent=true answers ok=false for unknown sessions instead of the permissive default.
	Absent bool
	mu     sync.Mutex
	rows   map[string]riskRow
}

type riskRow struct {
	band     safety.Band
	planHash string
}

// Set records the classifier's band + plan hash for (externalRef, actionID); actionID "" is the session default.
func (r *RiskAudits) Set(externalRef, actionID string, band safety.Band, planHash string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rows == nil {
		r.rows = map[string]riskRow{}
	}
	r.rows[externalRef+"\x00"+actionID] = riskRow{band: band, planHash: planHash}
}

// LatestFor implements runner.RiskAuditReader (exact pair first — exact=true — then the session default with
// exact=false). The permissive default (no rows, Absent=false) answers exact=true so legacy oracles keep the
// single-action path.
func (r *RiskAudits) LatestFor(_ context.Context, externalRef, actionID string) (safety.Band, string, bool, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if row, ok := r.rows[externalRef+"\x00"+actionID]; ok {
		return row.band, row.planHash, true, true, nil
	}
	if row, ok := r.rows[externalRef+"\x00"]; ok {
		return row.band, row.planHash, false, true, nil
	}
	if r.Absent {
		return safety.BandPollPause, "", false, false, nil
	}
	return safety.BandAuto, "", true, true, nil
}

// Executions is an in-memory CommitConfirmExecutionReader: nothing has executed unless Record was called.
type Executions struct {
	mu   sync.Mutex
	done map[string]bool
}

// Record marks (actionID, externalRef) as executed.
func (e *Executions) Record(actionID, externalRef string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done == nil {
		e.done = map[string]bool{}
	}
	e.done[actionID+"\x00"+externalRef] = true
}

// ExecutionFor implements the execution reader seam.
func (e *Executions) ExecutionFor(_ context.Context, actionID, externalRef string) (db.ForwardExecution, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done[actionID+"\x00"+externalRef] {
		return db.ForwardExecution{ActionID: actionID, ExternalRef: externalRef}, true, nil
	}
	return db.ForwardExecution{}, false, nil
}

// PlanSteps is an in-memory runner.PlanStepReader.
type PlanSteps struct {
	mu    sync.Mutex
	steps map[string]bool
}

// Add records actionID as a step of planID.
func (p *PlanSteps) Add(planID, actionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.steps == nil {
		p.steps = map[string]bool{}
	}
	p.steps[planID+"\x00"+actionID] = true
}

// HasStep implements runner.PlanStepReader.
func (p *PlanSteps) HasStep(_ context.Context, planID, actionID string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.steps[planID+"\x00"+actionID], nil
}
