package policy

// Policy-decision-surface hardening oracles (REQ-1522). Three properties are locked here:
//
//  1. bundle version — the content-derived identity of the loaded rule bundle is recorded on EVERY decision
//     (an allow/auto decision AND a deny decision), so the audit trail names exactly which rule set decided;
//  2. full matched-rule list — a decision surfaces the FULL, bounded, typed list of rules that matched
//     (deny-overrides provenance) while the resolved verdict is unchanged;
//  3. fail-to-deny on evaluator error — when the fixed Rego evaluator returns an ERROR (not a resolved
//     verdict), the decision fails closed to `deny` (refuse), never `approve`, never `auto`.
//
// (3) is driven white-box by injecting a failing evaluator through the unexported baseEvaluator seam (the same
// package-test injection pattern mode_test.go's failingLedger uses) — there is deliberately NO public path from
// input to an evaluator error, so a package test is the faithful lock.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
)

// errEvaluator is a baseEvaluator that always returns an error instead of a resolved verdict — it stands in for
// a broken/erroring fixed module so the fail-to-deny contract (REQ-1522) is lockable.
type errEvaluator struct{ err error }

func (e errEvaluator) evaluate(context.Context, []map[string]any) (regoResult, error) {
	return regoResult{}, e.err
}

// autoInput is a reversible, high-confidence, auto-banded action on the given host that SURVIVES every
// composition stage to `auto` (the allow decision used to prove bundle version rides an allow, not only a deny).
func autoInput(host string) EvalInput {
	return EvalInput{Host: host, Reversible: true, Confidence: 1.0, Band: safety.BandAuto, Mode: ModeSemiAuto}
}

// TestDecide_EvaluatorError_FailsClosedToDeny is the load-bearing fail-to-deny oracle (REQ-1522, INV-09): when
// the evaluator returns an ERROR, the decision is `deny` (refuse) — NEVER `approve`, NEVER `auto` — and the
// error is surfaced. The bundle version of the loaded rule set is still recorded on the refusal.
func TestDecide_EvaluatorError_FailsClosedToDeny(t *testing.T) {
	e := mustEngine(t, hostRule(t, "auto-1", "h1", VerdictAuto))
	wantBundle := e.BundleVersion()
	// Swap the real fixed evaluator for one that errors (white-box injection via the unexported seam).
	sentinel := errors.New("simulated OPA/Rego evaluator failure")
	e.eval = errEvaluator{err: sentinel}

	d, err := e.Decide(context.Background(), autoInput("h1"))

	if err == nil {
		t.Fatal("evaluator error was swallowed: Decide returned nil error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("surfaced error = %v, want it to wrap the evaluator error", err)
	}
	if d.Verdict() != VerdictDeny {
		t.Fatalf("evaluator-error verdict = %q, want DENY (fail closed) — never approve, never auto", d.Verdict())
	}
	// Explicit negative controls: the two permissive verdicts are BOTH forbidden on an evaluator error.
	if d.Verdict() == VerdictApprove {
		t.Fatal("evaluator error fell open to approve (a permissive verdict) — must fail closed to deny")
	}
	if d.Verdict() == VerdictAuto {
		t.Fatal("evaluator error fell open to auto — must fail closed to deny")
	}
	if d.BundleVersion() != wantBundle || wantBundle == "" {
		t.Fatalf("evaluator-error decision bundle version = %q, want the loaded %q", d.BundleVersion(), wantBundle)
	}
}

// TestDecide_BundleVersionOnAllowAndDeny proves the loaded bundle version rides EVERY decision (REQ-1522) —
// both an allow (auto) decision and a deny decision — and equals the engine's content-derived identity.
func TestDecide_BundleVersionOnAllowAndDeny(t *testing.T) {
	// Allow: a single auto rule that survives to auto.
	allowEng := mustEngine(t, hostRule(t, "allow", "h1", VerdictAuto))
	allow := decide(t, allowEng, autoInput("h1"))
	if allow.Verdict() != VerdictAuto {
		t.Fatalf("allow setup did not resolve to auto: got %q", allow.Verdict())
	}
	if allow.BundleVersion() == "" || allow.BundleVersion() != allowEng.BundleVersion() {
		t.Fatalf("allow decision bundle = %q, want engine bundle %q (non-empty)", allow.BundleVersion(), allowEng.BundleVersion())
	}

	// Deny (the short-circuit path): a deny rule on the same host.
	denyEng := mustEngine(t, hostRule(t, "deny", "h1", VerdictDeny))
	deny := decide(t, denyEng, EvalInput{Host: "h1"})
	if deny.Verdict() != VerdictDeny {
		t.Fatalf("deny setup did not resolve to deny: got %q", deny.Verdict())
	}
	if deny.BundleVersion() == "" || deny.BundleVersion() != denyEng.BundleVersion() {
		t.Fatalf("deny decision bundle = %q, want engine bundle %q (non-empty)", deny.BundleVersion(), denyEng.BundleVersion())
	}

	// Two different bundles must have different identities (the version is content-derived, not a constant).
	if allowEng.BundleVersion() == denyEng.BundleVersion() {
		t.Fatalf("distinct rule bundles share a version %q — the fingerprint is not content-derived", allowEng.BundleVersion())
	}
}

// TestBundleVersion_Deterministic proves the bundle version is a stable content fingerprint: identical rule
// data yields the SAME version and a change to any evaluated field yields a DIFFERENT one.
func TestBundleVersion_Deterministic(t *testing.T) {
	a := mustEngine(t, hostRule(t, "r", "h1", VerdictAuto))
	b := mustEngine(t, hostRule(t, "r", "h1", VerdictAuto))
	if a.BundleVersion() != b.BundleVersion() {
		t.Fatalf("same rule data produced different versions %q vs %q", a.BundleVersion(), b.BundleVersion())
	}
	c := mustEngine(t, hostRule(t, "r", "h1", VerdictDeny)) // verdict changed
	if a.BundleVersion() == c.BundleVersion() {
		t.Fatal("a changed verdict did not move the bundle version")
	}
	if !strings.HasPrefix(a.BundleVersion(), "sha256:") {
		t.Fatalf("bundle version %q is not the expected sha256-prefixed fingerprint", a.BundleVersion())
	}
}

// TestDecide_FullMatchedRuleListSurfaced proves a decision surfaces the FULL, typed list of matched rules
// (REQ-1522) while the deny-overrides OUTCOME is unchanged. An auto rule and a deny rule both match the action;
// the resolved verdict is deny (deny-overrides), and BOTH matched rules are surfaced with their verdicts.
func TestDecide_FullMatchedRuleListSurfaced(t *testing.T) {
	auto := hostRule(t, "auto-1", "h1", VerdictAuto)
	deny := hostRule(t, "deny-1", "h1", VerdictDeny)
	e := mustEngine(t, auto, deny)
	d := decide(t, e, EvalInput{Host: "h1"})

	// Outcome UNCHANGED: deny-overrides still wins.
	if d.Verdict() != VerdictDeny {
		t.Fatalf("outcome changed: verdict = %q, want deny (deny-overrides preserved)", d.Verdict())
	}

	got := d.MatchedRules()
	if len(got) != 2 {
		t.Fatalf("matched-rule list has %d entries, want the FULL 2 (auto + deny): %+v", len(got), got)
	}
	seen := map[string]Verdict{}
	for _, m := range got {
		seen[m.ID] = m.Verdict
	}
	if seen["auto-1"] != VerdictAuto {
		t.Fatalf("matched list missing auto-1:auto, got %+v", got)
	}
	if seen["deny-1"] != VerdictDeny {
		t.Fatalf("matched list missing deny-1:deny, got %+v", got)
	}

	// The returned slice is a defensive COPY — mutating it does not mutate the decision.
	got[0].ID = "mutated"
	if d.MatchedRules()[0].ID == "mutated" {
		t.Fatal("MatchedRules is not a defensive copy")
	}
}

// TestLedgerRowCarriesBundleAndMatched proves the SERIALIZED decision detail persisted to the tamper-evident
// governance ledger carries the bundle version + the full matched-rule list (REQ-1522) — with NO new column
// (the detail rides the existing GovDecision.Reason). This is the end-to-end persistence proof for hardening #1.
func TestLedgerRowCarriesBundleAndMatched(t *testing.T) {
	auto := hostRule(t, "auto-1", "h1", VerdictAuto)
	deny := hostRule(t, "deny-1", "h1", VerdictDeny)
	e := mustEngine(t, auto, deny)
	ledger := audit.NewLedger()
	ae := NewAuditedEngine(e, NewLedgerAuditSink(ledger))

	dec, err := ae.Decide(context.Background(), EvalInput{Host: "h1"})
	if err != nil {
		t.Fatalf("audited Decide: %v", err)
	}
	if ledger.Len() != 1 {
		t.Fatalf("ledger has %d rows, want exactly one", ledger.Len())
	}
	reason := ledger.Entries()[0].Reason
	if !strings.Contains(reason, "bundle="+dec.BundleVersion()) {
		t.Fatalf("ledger row does not record the bundle version: %q", reason)
	}
	if !strings.Contains(reason, "auto-1:auto") || !strings.Contains(reason, "deny-1:deny") {
		t.Fatalf("ledger row does not record the full matched-rule list: %q", reason)
	}
	if err := ledger.Verify(); err != nil {
		t.Fatalf("governance ledger chain broken: %v", err)
	}
}
