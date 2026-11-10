package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/policy"
)

// TG-666 part 4: a ladder transition becomes a `graduation:<kind>:<op_class>` governance row carrying from → to.
func TestALadderTransitionBecomesAGovernanceRow(t *testing.T) {
	var rows []audit.GovDecision
	rec := graduationTransitionRecorder(func(d audit.GovDecision) error { rows = append(rows, d); return nil })
	rec(policy.RecordResult{OpClass: "start-service", Outcome: policy.OutcomeVerifiedClean, From: policy.LevelAutoNotice, To: policy.LevelAuto, Promoted: true, Reason: "r"})
	rec(policy.RecordResult{OpClass: "start-service", Outcome: policy.OutcomeDeviated, From: policy.LevelAuto, To: policy.LevelApprove, Demoted: true, Reason: "r"})
	if len(rows) != 2 || rows[0].Decision != "graduation:promote:start-service" || rows[1].Decision != "graduation:demote:start-service" {
		t.Fatalf("want promote then demote rows, got %+v", rows)
	}
	if !strings.Contains(rows[0].Reason, policy.LevelAutoNotice.String()) || !strings.Contains(rows[0].Reason, policy.LevelAuto.String()) {
		t.Fatalf("the row must carry from → to, got %q", rows[0].Reason)
	}
	// A failed append is logged, never a panic: the rung change already landed.
	graduationTransitionRecorder(func(audit.GovDecision) error { return errors.New("ledger down") })(policy.RecordResult{OpClass: "x", Promoted: true})
}
