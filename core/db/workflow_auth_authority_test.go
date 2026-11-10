package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
)

// TG-660: the three pgx readers/writers behind the signed envelope's replay guard and ExecuteActivity's
// authority re-derivation, against a REAL PostgreSQL (TG_TEST_DSN, the migrated fixture). A pgx fake would hide
// exactly the defects that matter here: the LIKE/position anchoring on the reason column, ON CONFLICT semantics,
// the band enum round-trip.

func authorityPool(t *testing.T) (*Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("TG_TEST_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_DSN to a migrated database to run the TG-660 authority-record tests")
	}
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p, ctx
}

// The nonce claim is first-wins and atomic in one statement.
func TestWorkflowAuthNonceClaimIsFirstWins(t *testing.T) {
	p, ctx := authorityPool(t)
	s := NewWorkflowAuthNonceStore(p)
	nonce := fmt.Sprintf("tg660-%d", time.Now().UnixNano())
	fresh, err := s.Claim(ctx, nonce, "modetransition", time.Now().Add(time.Hour))
	if err != nil || !fresh {
		t.Fatalf("first claim must be fresh: fresh=%v err=%v", fresh, err)
	}
	again, err := s.Claim(ctx, nonce, "modetransition", time.Now().Add(time.Hour))
	if err != nil || again {
		t.Fatalf("second claim of the same nonce must NOT be fresh (a replay): fresh=%v err=%v", again, err)
	}
	other, err := s.Claim(ctx, nonce+"-b", "vote", time.Now().Add(time.Hour))
	if err != nil || !other {
		t.Fatalf("a different nonce is fresh: fresh=%v err=%v", other, err)
	}
}

// ApprovalFor finds exactly the approval rows RecordVoteActivity writes, bound to the session in the reason.
func TestLedgerApprovalForBindsActionAndSession(t *testing.T) {
	p, ctx := authorityPool(t)
	store := NewLedgerStore(p)
	seq, hash, err := store.Tail(ctx)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	led := audit.NewLedgerFromTail(seq, hash).WithSink(store)
	stamp := time.Now().UnixNano()
	action := fmt.Sprintf("act-660-%d", stamp)
	ref := fmt.Sprintf("TG-660-%d", stamp)
	// a deny on this action/session, an approve on the SAME action in ANOTHER session, then the real approve.
	for _, d := range []audit.GovDecision{
		{Decision: "human:deny", Reason: "external_ref=" + ref + " voter=zoe", ActionID: action, Withheld: true},
		{Decision: "human:approve", Reason: "external_ref=" + ref + "-other voter=zoe", ActionID: action},
	} {
		if _, err := led.Append(d); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if _, ok, err := store.ApprovalFor(ctx, action, ref); err != nil || ok {
		t.Fatalf("a deny, and an approve in another session, must NOT count as this session's approval: ok=%v err=%v", ok, err)
	}
	if _, err := led.Append(audit.GovDecision{Decision: "human:approve", Reason: "external_ref=" + ref + " voter=kyriakos", ActionID: action}); err != nil {
		t.Fatalf("append approve: %v", err)
	}
	dec, ok, err := store.ApprovalFor(ctx, action, ref)
	if err != nil || !ok || dec != "human:approve" {
		t.Fatalf("the session's approve row must be found: dec=%q ok=%v err=%v", dec, ok, err)
	}
	// the fallback approver's row counts too, and a prefix-colliding external_ref does not.
	action2 := action + "-auto"
	if _, err := led.Append(audit.GovDecision{Decision: "fallback:autonomous-approve:ood-novel-incident",
		Reason: "external_ref=" + ref + "1 voter=tg:fallback-approver", ActionID: action2}); err != nil {
		t.Fatalf("append fallback: %v", err)
	}
	if _, ok, _ := store.ApprovalFor(ctx, action2, ref); ok {
		t.Fatal("external_ref is matched as a whole token — a prefix collision (ref vs ref1) must not count")
	}
	dec, ok, err = store.ApprovalFor(ctx, action2, ref+"1")
	if err != nil || !ok || dec != "fallback:autonomous-approve:ood-novel-incident" {
		t.Fatalf("the fallback approver's row must be found: dec=%q ok=%v err=%v", dec, ok, err)
	}
}

// LatestFor reads the classifier's row for the exact (session, action), falls back to the session's latest row for
// a plan step, and answers ok=false for an unclassified session; the band enum round-trips.
func TestRiskAuditLatestForBindsBandAndPlan(t *testing.T) {
	p, ctx := authorityPool(t)
	s := NewRiskAuditStore(p)
	stamp := time.Now().UnixNano()
	ref := fmt.Sprintf("TG-660-ra-%d", stamp)
	action := fmt.Sprintf("act-ra-%d", stamp)
	if _, _, _, ok, err := s.LatestFor(ctx, ref, action); err != nil || ok {
		t.Fatalf("an unclassified session must answer ok=false: ok=%v err=%v", ok, err)
	}
	if err := s.PersistRiskAudit(audit.RiskAudit{ExternalRef: ref, RiskLevel: "medium", Band: safety.BandPollPause,
		PlanHash: "plan#1", ActionID: action, SchemaVersion: 1}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	band, plan, exact, ok, err := s.LatestFor(ctx, ref, action)
	if err != nil || !ok || !exact || band != safety.BandPollPause || plan != "plan#1" {
		t.Fatalf("exact row: band=%v plan=%q exact=%v ok=%v err=%v", band, plan, exact, ok, err)
	}
	// a plan step: no row for ITS action id, so the session's latest row answers — marked exact=false, which the
	// caller admits only for an action it has bound to the plan
	band, plan, exact, ok, err = s.LatestFor(ctx, ref, action+"-step2")
	if err != nil || !ok || exact || band != safety.BandPollPause || plan != "plan#1" {
		t.Fatalf("session fallback: band=%v plan=%q exact=%v ok=%v err=%v", band, plan, exact, ok, err)
	}
	// a later re-classification to AUTO is the latest row
	time.Sleep(2 * time.Millisecond)
	if err := s.PersistRiskAudit(audit.RiskAudit{ExternalRef: ref, RiskLevel: "low", Band: safety.BandAuto,
		PlanHash: "plan#2", ActionID: action, SchemaVersion: 1}); err != nil {
		t.Fatalf("persist 2: %v", err)
	}
	band, plan, exact, ok, err = s.LatestFor(ctx, ref, action)
	if err != nil || !ok || !exact || band != safety.BandAuto || plan != "plan#2" {
		t.Fatalf("latest row must win: band=%v plan=%q exact=%v ok=%v err=%v", band, plan, exact, ok, err)
	}
	if bandFromText("AUTO_NOTICE") != safety.BandAutoNotice || bandFromText("garbage") != safety.BandPollPause {
		t.Fatal("bandFromText must invert String() and fail closed on an unknown spelling")
	}
}

// HasStep binds a plan's approval to exactly the actions the plan workflow recorded; a missing plan is false.
func TestTransactionPlanHasStepBindsActionsToThePlan(t *testing.T) {
	p, ctx := authorityPool(t)
	s := NewTransactionPlanStore(p)
	stamp := time.Now().UnixNano()
	planID := fmt.Sprintf("plan-660-%d", stamp)
	stepA, stepB := fmt.Sprintf("act-a-%d", stamp), fmt.Sprintf("act-b-%d", stamp)
	t.Cleanup(func() {
		_, _ = p.Exec(ctx, "DELETE FROM transaction_plan_step WHERE plan_id = $1", planID)
		_, _ = p.Exec(ctx, "DELETE FROM transaction_plan WHERE plan_id = $1", planID)
	})
	if ok, err := s.HasStep(ctx, planID, stepA); err != nil || ok {
		t.Fatalf("an unrecorded plan must answer false: ok=%v err=%v", ok, err)
	}
	if err := s.Create(ctx, planID, "recipe-660", fmt.Sprintf("TG-660-plan-%d", stamp), []PlanStep{
		{Ordinal: 1, ActionID: stepA, OpClass: "restart-service"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if ok, err := s.HasStep(ctx, planID, stepA); err != nil || !ok {
		t.Fatalf("the recorded step must be found: ok=%v err=%v", ok, err)
	}
	if ok, err := s.HasStep(ctx, planID, stepB); err != nil || ok {
		t.Fatalf("an action outside the plan must NOT borrow its approval: ok=%v err=%v", ok, err)
	}
	if ok, err := s.HasStep(ctx, planID+"-other", stepA); err != nil || ok {
		t.Fatalf("the step of one plan is not a step of another: ok=%v err=%v", ok, err)
	}
}

// A MALFORMED prediction row (a schema version this binary does not know) is unreadable, never an empty
// prediction: ExecuteActivity refuses it as prediction-unreadable.
func TestPredictionGetRefusesAMalformedRow(t *testing.T) {
	p, ctx := authorityPool(t)
	s := NewPredictionStore(p)
	stamp := time.Now().UnixNano()
	planHash := fmt.Sprintf("plan-660-malformed-%d", stamp)
	t.Cleanup(func() { _, _ = p.Exec(ctx, "DELETE FROM infragraph_prediction WHERE plan_hash = $1", planHash) })
	if _, err := p.Exec(ctx, `
		INSERT INTO infragraph_prediction (plan_hash, kind, action_id, target_host, site, external_ref, prediction_hash, schema_version)
		VALUES ($1, 'action', $2, 'web01', 'nl', $3, 'pred#x', 9999)`,
		planHash, fmt.Sprintf("act-%d", stamp), fmt.Sprintf("TG-660-%d", stamp)); err != nil {
		t.Fatalf("seed malformed row: %v", err)
	}
	rec, ok, err := s.Get(ctx, planHash)
	if err == nil || ok {
		t.Fatalf("a row with an unknown schema version must be an ERROR, not a prediction: rec=%+v ok=%v err=%v", rec, ok, err)
	}
}
