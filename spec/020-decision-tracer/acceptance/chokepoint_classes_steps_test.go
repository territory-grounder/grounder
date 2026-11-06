package acceptance

import (
	"context"
	"errors"
	"fmt"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/regime"
)

// REQ-2001 acceptance binding: every decision chokepoint's audit emit is a nil-safe side-write, and an ABSENT
// sink is governed by TWO classes that differ deliberately — the OBSERVE-ONLY emitters degrade to a no-op (a
// missing observability write must not stop a sound decision) while the ACTUATION-AUTHORITY records FAIL
// CLOSED (an unrecordable actuation must refuse rather than proceed unobserved, INV-19). This drives the REAL
// audit code: policy.LedgerAuditSink (observe-only) and regime.Audit (authority).
func init() {
	stepRegistrars = append(stepRegistrars, registerChokepointClassesSteps)
}

type chokepointWorld struct {
	presentErr         error
	presentCount       int
	observeAbsentErr   error
	authorityAbsentErr error
}

func registerChokepointClassesSteps(sc *godog.ScenarioContext) {
	w := &chokepointWorld{}
	ctx := context.Background()

	sc.Step(`^the decision path runs classify each interceptor gate each ReAct cycle policy Decide credential Resolve regime select and verify$`, func() error {
		return nil
	})

	sc.Step(`^the trace sink is present and when it is absent$`, func() error {
		// PRESENT sink: a wired authority record appends its row — the nil-safe side-write lands.
		sink := regime.NewMemAuditSink()
		w.presentErr = regime.NewAudit(sink, audit.NewLedger()).RecordResolution(ctx, regime.ResolutionRow{Target: "web01", Outcome: regime.OutcomeRefused})
		res, _, _ := sink.Counts()
		w.presentCount = res
		// ABSENT sink, OBSERVE-ONLY class (policy Decide audit projected to the ledger): a nil ledger is a no-op.
		w.observeAbsentErr = policy.NewLedgerAuditSink(nil).AppendPolicyDecision(ctx, policy.PolicyDecision{})
		// ABSENT sink, ACTUATION-AUTHORITY class (regime resolution): a nil sink fails closed.
		w.authorityAbsentErr = regime.NewAudit(nil, audit.NewLedger()).RecordResolution(ctx, regime.ResolutionRow{Target: "web01", Outcome: regime.OutcomeRefused})
		return nil
	})

	sc.Step(`^each boundary emits one nil-safe side-write when the sink is present and when the sink is absent the observe-only emitters degrade to a no-op leaving the decision unchanged while the actuation-authority records fail closed refusing to proceed unobserved$`, func() error {
		// Present ⇒ the side-write lands (one recorded row, no error).
		if w.presentErr != nil {
			return fmt.Errorf("a wired boundary must emit its side-write, got %v", w.presentErr)
		}
		if w.presentCount != 1 {
			return fmt.Errorf("the present sink must record exactly one row, got %d", w.presentCount)
		}
		// Observe-only + absent ⇒ degrade to a no-op: no error, so the decision path is unaffected.
		if w.observeAbsentErr != nil {
			return fmt.Errorf("an observe-only emitter must degrade to a no-op when the sink is absent, got %v", w.observeAbsentErr)
		}
		// Authority + absent ⇒ fail closed: refuse rather than proceed unobserved.
		if !errors.Is(w.authorityAbsentErr, regime.ErrAuditNotWired) {
			return fmt.Errorf("an actuation-authority record must FAIL CLOSED (ErrAuditNotWired) when the sink is absent, got %v", w.authorityAbsentErr)
		}
		return nil
	})
}
