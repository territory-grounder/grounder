package actuate

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// TestPreExistingUnrelatedAlertDoesNotFalseDeviate is the TG-148 regression: an UNRELATED estate alert already
// firing BEFORE the action (on a host the prediction never named) must NOT be misread as a cascade surprise.
// The post-state Observe is estate-WIDE, so without the pre-execute baseline this would false-DEVIATE — demoting
// the op-class + tripping the breaker on a SUCCESSFUL heal. Same alert pre+post ⇒ MATCH; a control where it
// appears only POST still DEVIATES (the fix must not blind real cascades).
func TestPreExistingUnrelatedAlertDoesNotFalseDeviate(t *testing.T) {
	ctx := context.Background()
	unrelated := []verify.ObservedAlert{{Host: "unrelated-db07", Rule: "DiskFull", Site: "gr"}}

	// (a) pre-existing: the unrelated alert is present BEFORE and AFTER the action → not this action's cascade → MATCH.
	preExisting := goodRequest(t)
	preExisting.Observe = func(context.Context) []verify.ObservedAlert { return unrelated } // identical pre + post
	out, err := NewInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).Do(ctx, preExisting)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || out.Verdict != safety.VerdictMatch {
		t.Fatalf("a pre-existing UNRELATED alert must NOT deviate (TG-148: it fired before the action), got %+v", out)
	}

	// (b) control — the SAME unrelated alert appears only AFTER the action (a genuine new surprise) → DEVIATION.
	appeared := goodRequest(t)
	call := 0
	appeared.Observe = func(context.Context) []verify.ObservedAlert {
		call++
		if call == 1 {
			return []verify.ObservedAlert{} // pre: quiet
		}
		return unrelated // post: it appeared — a real cascade candidate
	}
	out2, err := NewInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).Do(ctx, appeared)
	if err != nil {
		t.Fatal(err)
	}
	if out2.Verdict != safety.VerdictDeviation {
		t.Fatalf("a NEW post-action alert on an unpredicted host must still DEVIATE (the fix must not blind real cascades), got %v", out2.Verdict)
	}
}

// TestDeviationLedgerReasonCarriesBreakdown proves the execute:deviation ledger record enriches its reason with
// the structured verdict breakdown (TG-148 diagnostic): the surprise host(s) that TRIGGERED the deviation are
// NAMED, so a false-deviation — e.g. a pre-existing unrelated estate alert misread as a cascade surprise, since
// the post-state Observe is estate-wide — is traceable post-hoc to the exact unpredicted host instead of an
// opaque "deviation". action_verdict persists only the verdict enum; this reason is the diagnosable record. The
// derived verdict + the demote/breaker reactions are unchanged (observability only).
func TestDeviationLedgerReasonCarriesBreakdown(t *testing.T) {
	ctx := context.Background()
	ledger := audit.NewLedger()
	i := NewInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, ledger) // mutation ON (test-only)
	out, err := i.Do(ctx, deviationRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || out.Verdict != safety.VerdictDeviation {
		t.Fatalf("precondition: the action must execute and verify a deviation, got %+v", out)
	}
	var reason string
	for _, e := range ledger.Entries() {
		if strings.Contains(e.Decision, "execute:deviation") {
			reason = e.Reason
		}
	}
	if reason == "" {
		t.Fatal("no execute:deviation ledger record found")
	}
	// deviationRequest observes a surprise host 'surprise99' — the deviation trigger MUST be named in the reason.
	if !strings.Contains(reason, "DEVIATION") || !strings.Contains(reason, "surprise99") {
		t.Fatalf("the deviation reason must name the surprise host(s) that triggered it (TG-148 diagnostic), got: %q", reason)
	}
}

// TestNonDeviationLedgerReasonUnchanged proves the enrichment fires ONLY on a deviation: a clean match records
// the plain "governed actuation executed" reason (no breakdown noise on the happy path).
func TestNonDeviationLedgerReasonUnchanged(t *testing.T) {
	ctx := context.Background()
	ledger := audit.NewLedger()
	i := NewInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, ledger)
	out, err := i.Do(ctx, goodRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || out.Verdict != safety.VerdictMatch {
		t.Fatalf("precondition: the action must execute and verify a match, got %+v", out)
	}
	for _, e := range ledger.Entries() {
		if strings.Contains(e.Decision, "execute:match") {
			if e.Reason != "governed actuation executed" {
				t.Fatalf("a match must keep the plain reason (no deviation breakdown), got: %q", e.Reason)
			}
			return
		}
	}
	t.Fatal("no execute:match ledger record found")
}
