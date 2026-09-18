package policy

// TG-509 present-not-reaching sweep / REQ-1518: a policy authorization decision must reach the tamper-EVIDENT
// hash-chained governance ledger, not only the (tamper-resistant) policy_decision table. teeAuditSink fans each
// decision to BOTH sinks. These oracles prove the fan-out, the best-effort-across-sinks SYMMETRY (a failing sink
// must not create the table/ledger split REQ-1518 forbids), the nil-drop, and that AuditedEngine + the tee lands
// EVERY Decide in EVERY sink — closing the gap where policy authorization alone skipped the ledger.

import (
	"context"
	"errors"
	"testing"
)

// aDecision resolves one real PolicyDecision through the engine (the tee is content-agnostic, but a real
// decision keeps the oracle honest against the production shape).
func aDecision(t *testing.T) PolicyDecision {
	t.Helper()
	e := mustEngine(t, hostRule(t, "allow-h1", "h1", VerdictAuto))
	d, err := e.Decide(context.Background(), autoInput("h1"))
	if err != nil {
		t.Fatalf("build decision: %v", err)
	}
	return d
}

// TestTeeAuditSinkFansOutToEverySink: a decision reaches ALL configured sinks, not just the first.
// KILLING MUTATION: drop the loop in AppendPolicyDecision (offer only sinks[0]) → sink B Len()==0 → RED.
func TestTeeAuditSinkFansOutToEverySink(t *testing.T) {
	a, b := NewMemAuditSink(), NewMemAuditSink()
	if err := NewTeeAuditSink(a, b).AppendPolicyDecision(context.Background(), aDecision(t)); err != nil {
		t.Fatalf("tee append: %v", err)
	}
	if a.Len() != 1 || b.Len() != 1 {
		t.Fatalf("tee did not fan out to both sinks: a=%d b=%d, want 1/1", a.Len(), b.Len())
	}
}

// TestTeeAuditSinkIsBestEffortAcrossSinks: a failing sink must NOT stop the others, and the joined error must
// surface so AuditedEngine.emit can trace it. A short-circuit here is the exact table/ledger split REQ-1518
// forbids. KILLING MUTATION: return on the first sink error (short-circuit) → healthy sink B Len()==0 → RED.
func TestTeeAuditSinkIsBestEffortAcrossSinks(t *testing.T) {
	down := errors.New("sink A durable write down")
	a := NewMemAuditSink().WithAppendError(down) // fails, but still records the OFFER (Len counts offered)
	b := NewMemAuditSink()                        // healthy
	err := NewTeeAuditSink(a, b).AppendPolicyDecision(context.Background(), aDecision(t))
	if !errors.Is(err, down) {
		t.Fatalf("tee must surface the failing sink's error (joined), got %v", err)
	}
	if b.Len() != 1 {
		t.Fatalf("a failing sink A stopped healthy sink B (Len=%d) — the table/ledger split REQ-1518 forbids", b.Len())
	}
	if a.Len() != 1 {
		t.Fatalf("sink A was not even OFFERED the record (Len=%d)", a.Len())
	}
}

// TestTeeAuditSinkDropsNilSinks: a nil sink argument is dropped, not wrapped — no nil-deref in the fan-out, and
// the real sink still receives the record.
func TestTeeAuditSinkDropsNilSinks(t *testing.T) {
	b := NewMemAuditSink()
	if err := NewTeeAuditSink(nil, b, nil).AppendPolicyDecision(context.Background(), aDecision(t)); err != nil {
		t.Fatalf("tee with nil sinks append: %v", err)
	}
	if b.Len() != 1 {
		t.Fatalf("tee dropped the real sink along with the nils: b=%d, want 1", b.Len())
	}
}

// TestAuditedEngineWithTeeAuditsEveryDecisionToEverySink is the production shape (the cmd/worker wiring):
// AuditedEngine over a tee of [table-sink, ledger-sink] must land EVERY Decide in BOTH — the persistence half
// AND the ledger half REQ-1518 names — so policy authorization is no longer the governance-decision class that
// skips the ledger. KILLING MUTATION: revert the wiring to a single sink → the second sink Len()==0 → RED.
func TestAuditedEngineWithTeeAuditsEveryDecisionToEverySink(t *testing.T) {
	e := mustEngine(t, hostRule(t, "allow-h1", "h1", VerdictAuto))
	table, ledger := NewMemAuditSink(), NewMemAuditSink()
	ae := NewAuditedEngine(e, NewTeeAuditSink(table, ledger))
	if _, err := ae.Decide(context.Background(), autoInput("h1")); err != nil {
		t.Fatalf("audited decide: %v", err)
	}
	if table.Len() != 1 || ledger.Len() != 1 {
		t.Fatalf("AuditedEngine+tee did not audit to BOTH sinks: table=%d ledger=%d, want 1/1", table.Len(), ledger.Len())
	}
}
