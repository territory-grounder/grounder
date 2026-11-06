package main

import (
	"os"
	"strings"
	"testing"
)

// TG-80 P1.1 wiring guard, the workerMainSource/ledger_airgap_wired_test.go house pattern ("implemented ≠
// reachable" — TG-378's advisory, and the exact shape core/audit/anchor.go's own P1#1 predecessor left open:
// code-complete math with nothing durably landing it in Temporal). Source assertions with a vacuity floor and
// a comment-stripper, so prose describing the wiring cannot satisfy these — only the real call sites can.
//
// KILLING MUTATION: delete `wireLedgerAnchorTemporalWitness(c, w, dbPool)` from main() —
// TestLedgerAnchorTemporalWitnessIsWired fails naming the missing call site. Restore → green.

// ledgerAnchorWiringSource returns ledger_anchor_wiring.go with comment lines stripped, mirroring
// workerMainSource's stripper + vacuity floor (guest_liveness_wire_test.go) for the file that actually
// contains the registration + submission code this guard pins.
func ledgerAnchorWiringSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("ledger_anchor_wiring.go")
	if err != nil {
		t.Fatalf("read ledger_anchor_wiring.go: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		out = append(out, line)
	}
	src := strings.Join(out, "\n")
	if len(src) < 2_000 {
		t.Fatalf("VACUITY FLOOR: ledger_anchor_wiring.go stripped to %d bytes — every assertion below would pass on a stub", len(src))
	}
	return src
}

// TestLedgerAnchorTemporalWitnessIsWired pins the full chain from main() to the real worker registration and
// the real Temporal submission — the three places TG-80 P1.1 would ship UNREACHABLE if any one silently
// regressed: called from boot, registered on the runner worker, and actually armed (recorder + verifier both).
func TestLedgerAnchorTemporalWitnessIsWired(t *testing.T) {
	main := workerMainSource(t)
	if !strings.Contains(main, "wireLedgerAnchorTemporalWitness(c, w, dbPool)") {
		t.Fatal("main() no longer calls wireLedgerAnchorTemporalWitness — the Temporal-native ledger-HEAD " +
			"witness (TG-80 P1.1) is code-complete but unreachable at boot")
	}

	wiring := ledgerAnchorWiringSource(t)
	// The workflow+activity must be registered on the worker `w` — a Temporal witness that is never
	// registered starts (ExecuteWorkflow succeeds, the server queues the task) but never COMPLETES, silently
	// stuck "Running" forever, which is a worse failure than an obvious boot error.
	if !strings.Contains(wiring, "w.RegisterWorkflow(ledgeranchor.WitnessAnchorWorkflow)") {
		t.Error("WitnessAnchorWorkflow is not registered on the runner worker — a submitted witness would never complete")
	}
	if !strings.Contains(wiring, "w.RegisterActivity(ledgeranchor.WitnessAnchorActivity)") {
		t.Error("WitnessAnchorActivity is not registered on the runner worker — a submitted witness would never complete")
	}
	// The RECORDING half must actually be armed (not just defined) — the goroutine that submits anchors.
	if !strings.Contains(wiring, "ledgeranchor.RunPeriodically(context.Background(), recordJob, d,") {
		t.Error("the Temporal recording loop (ledgeranchor.RunPeriodically over the Temporal-backed sink) is not armed")
	}
	// The CONSUMING half must also be armed — recording without checking is exactly the TG-509 "present, not
	// reaching" shape this package's own doc warns against repeating for the Temporal side.
	if !strings.Contains(wiring, "ledgeranchor.RunTemporalVerifyPeriodically(context.Background(), verifyJob, d,") {
		t.Error("the Temporal cross-check loop (ledgeranchor.RunTemporalVerifyPeriodically) is not armed — anchors would be witnessed but never checked")
	}
	// The sink/reader driving both loops must be the SAME *TemporalWitness built from the real Temporal
	// client `c` — not a nil/stub value that would make either loop a no-op error generator.
	if !strings.Contains(wiring, "&ledgeranchor.TemporalWitness{Client: c, TaskQueue: tg.TaskQueueRunner, Domain: audit.DomainGovernanceLedger}") {
		t.Error("the TemporalWitness sink is not built from the real Temporal client/task-queue/domain — check the wiring was not pointed at a stub")
	}
}

// TestLedgerAnchorTemporalWitnessCannotOverwritePriorSeq pins the load-bearing safety property witness.go's
// doc claims: even the LEGITIMATE recorder cannot start a second execution under an already-witnessed
// (domain, seq) id — Temporal itself refuses it (REJECT_DUPLICATE), which is what makes "already started"
// safe to treat as idempotent success rather than a retry that could land a DIFFERENT payload under the same
// id. A regression to ALLOW_DUPLICATE would silently drop this guarantee while every existing test (which
// only ever witnesses each seq once) would keep passing — so it is pinned here at the source.
func TestLedgerAnchorTemporalWitnessCannotOverwritePriorSeq(t *testing.T) {
	b, err := os.ReadFile("../../temporal/ledgeranchor/witness.go")
	if err != nil {
		t.Fatalf("read witness.go: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, "WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE") {
		t.Fatal("TemporalWitness.Record no longer uses REJECT_DUPLICATE — a re-run could silently overwrite a previously-witnessed seq under the same Workflow ID")
	}
}
