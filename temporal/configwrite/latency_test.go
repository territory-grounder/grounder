package configwrite

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

// LATENCY ORACLES FOR THE SEALED-SECRET WRITE (TG-277).
//
// THE INCIDENT. On 2026-08-04 an administrator onboarding credentials on dc1tg01 saw two of four
// sealed-secret writes fail: one hit the activity's 15s StartToCloseTimeout, and the console reported a
// 503 that distinguished nothing. The ticket concluded the hash-chained Ledger.Append was taking ~10s.
//
// WHAT THE MEASUREMENT SAID. Six consecutive SecretPutWorkflow probes run live against the same ledger
// (seq ~8800) completed the whole workflow in 40-43ms, with PutSecretActivity itself at 11-13ms. The
// ~10.5s came from Temporal WORKFLOW-task timeouts (10.005s, twice) while the server's own Postgres was
// logging "Failed to start transaction: context deadline exceeded". The activity's work was never slow.
//
// WHY IT WAS STILL A DEFECT. The activity was UNBOUNDED and UNMEASURED. Ledger.Append reached a pgx
// INSERT that ran on context.Background(), so the activity's deadline never got there; a stalled
// substrate could hold step one until the database answered, burning the whole 15s under
// MaximumAttempts 1, and the activity emitted nothing that would say which step it was. That is why the
// ticket blamed a 12ms step, and it is what these tests hold shut.
//
// ★ A TEST THAT ONLY ASSERTS "THE WRITE COMPLETES" PROVES NOTHING HERE. Every oracle below asserts a
// LATENCY BOUND or asserts that the measurement exists.

// stalledLedger models the measured failure: the durable chain write does not answer because the
// substrate has stalled. It respects the context it is handed — which is the whole point, since before
// TG-277 the pgx sink was handed none.
type stalledLedger struct{}

func (stalledLedger) AppendContext(ctx context.Context, _ audit.GovDecision) (audit.LedgerEntry, error) {
	<-ctx.Done()
	return audit.LedgerEntry{}, ctx.Err()
}

func secretReq(name string) SecretRequest {
	return SecretRequest{
		Name: name, Ciphertext: []byte("ciphertext"), Nonce: []byte("nonce"),
		WrappedDEK: []byte("wrapped"), DEKNonce: []byte{},
		Purpose: "onboarding", Rationale: "TG-277 latency oracle", Operator: "kyriakos",
	}
}

// TestPutSecretRefusesInsideItsStepBudgetWhenTheLedgerStalls is the defect as a latency bound.
//
// The activity runs under the REAL StartToCloseTimeout from activityOpts, so the bound being asserted is
// the operator-visible one: a stalled step must refuse inside its own budget instead of consuming the
// whole activity allowance and surfacing as an opaque Temporal timeout.
func TestPutSecretRefusesInsideItsStepBudgetWhenTheLedgerStalls(t *testing.T) {
	const budget = 200 * time.Millisecond
	acts := &Activities{D: Deps{Ledger: stalledLedger{}, Secrets: &memSecrets{}, StepBudget: budget}}

	startToClose := activityOpts().StartToCloseTimeout
	ctx, cancel := context.WithTimeout(context.Background(), startToClose)
	defer cancel()

	start := time.Now()
	_, err := acts.PutSecretActivity(ctx, secretReq("librenms.token"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("a sealed-secret write whose ledger step never answered reported SUCCESS — the console " +
			"would tell the administrator the credential is stored when no row exists")
	}
	// THE LATENCY BOUND. 4x the budget is generous scheduling slack; the failure mode being excluded is
	// "elapsed grows to startToClose", which is ~75x this bound.
	if bound := 4 * budget; elapsed > bound {
		t.Fatalf("a stalled ledger step held the sealed-secret write for %s (bound %s, step budget %s): the "+
			"administrator waits the FULL %s StartToClose on the path that onboards EVERY credential and "+
			"then gets a 503 that names nothing — which is exactly how TG-277 came to blame a step that "+
			"measures ~12ms live", elapsed, bound, budget, startToClose)
	}
	if !strings.Contains(err.Error(), StepLedgerAppend) {
		t.Fatalf("the refusal does not name the step that blew its budget: %v — an unattributable timeout on "+
			"this lane is what sent TG-277 after the wrong suspect", err)
	}
}

// TestPutSecretRecordsEveryStepLatency is the vacuity floor on the instrumentation itself: the observer
// must receive all three steps, named, or a future timeout is once again unattributable. Asserting the
// exact expected step names means a run that measured nothing (or measured a step that no longer exists)
// fails rather than passing silently.
func TestPutSecretRecordsEveryStepLatency(t *testing.T) {
	var (
		gotOp    string
		gotSteps []StepLatency
		gotTotal time.Duration
		gotErr   error
		calls    int
	)
	acts, _, _, _ := rig()
	acts.D.Observe = func(op string, steps []StepLatency, total time.Duration, err error) {
		calls++
		gotOp, gotSteps, gotTotal, gotErr = op, steps, total, err
	}

	if _, err := acts.PutSecretActivity(context.Background(), secretReq("librenms.token")); err != nil {
		t.Fatalf("put: %v", err)
	}

	if calls != 1 {
		t.Fatalf("the latency observer fired %d times, want exactly 1 — a governed write that reports no "+
			"measurement leaves the next 15s timeout as unattributable as TG-277's was", calls)
	}
	want := []string{StepLedgerAppend, StepSchemaStamp, StepStoreWrite}
	if len(gotSteps) != len(want) {
		t.Fatalf("recorded %d steps %+v, want the %d named steps %v — the whole point of the record is that "+
			"an operator can see WHICH step took the time", len(gotSteps), gotSteps, len(want), want)
	}
	for i, w := range want {
		if gotSteps[i].Step != w {
			t.Fatalf("step %d recorded as %q, want %q — a mislabelled step misdirects the next investigation "+
				"the way TG-277 was misdirected", i, gotSteps[i].Step, w)
		}
	}
	if gotOp != "secret:put" || gotErr != nil || gotTotal <= 0 {
		t.Fatalf("measurement is not usable: op=%q total=%s err=%v — a total of zero means nothing was timed",
			gotOp, gotTotal, gotErr)
	}
}

// TestFailedWriteStillReportsItsStepLatencies: the run that matters most for diagnosis is the one that
// FAILED. If the observer only fires on success, the timeout leaves nothing behind — which is the state
// TG-277 was investigated in.
func TestFailedWriteStillReportsItsStepLatencies(t *testing.T) {
	const budget = 100 * time.Millisecond
	var gotSteps []StepLatency
	var gotErr error
	acts := &Activities{D: Deps{
		Ledger: stalledLedger{}, Secrets: &memSecrets{}, StepBudget: budget,
		Observe: func(_ string, steps []StepLatency, _ time.Duration, err error) { gotSteps, gotErr = steps, err },
	}}

	ctx, cancel := context.WithTimeout(context.Background(), activityOpts().StartToCloseTimeout)
	defer cancel()
	if _, err := acts.PutSecretActivity(ctx, secretReq("librenms.token")); err == nil {
		t.Fatal("the stalled write did not fail")
	}
	if len(gotSteps) == 0 || gotErr == nil {
		t.Fatalf("a FAILED sealed-secret write reported no step latencies (%+v) — the failing run is the one "+
			"an operator needs the numbers from", gotSteps)
	}
	if gotSteps[0].Step != StepLedgerAppend || gotSteps[0].Took < budget {
		t.Fatalf("the failing step is not the one recorded as expensive: %+v — the record must point at the "+
			"step that actually stalled", gotSteps)
	}
}

// TestStepBudgetsFitInsideTheActivityTimeout keeps the two numbers in the correct ORDER. If the three step
// budgets could add up to the StartToCloseTimeout, Temporal's opaque timeout could fire first and the
// per-step refusal — the entire point of TG-277 — would never be the message the operator sees.
func TestStepBudgetsFitInsideTheActivityTimeout(t *testing.T) {
	const stepsPerWrite = 3 // ledger append, schema stamp, store write
	total := stepsPerWrite * DefaultStepBudget
	startToClose := activityOpts().StartToCloseTimeout
	if total >= startToClose {
		t.Fatalf("%d step budgets of %s total %s, which does not fit inside the %s StartToCloseTimeout: a "+
			"stalled write would hit Temporal's timeout FIRST and report nothing about which step stalled, "+
			"restoring exactly the blindness TG-277 was filed under",
			stepsPerWrite, DefaultStepBudget, total, startToClose)
	}
}
