package db

// TG-191 (epic TG-187) — the MISSION GUARDRAIL as an auditable number. An auto-heal that acts WITHOUT a
// committed prediction, or whose outcome core/verify never graded, traded the falsifiable core for raw
// A5/A3 breadth: DRIFT. LoopBypass has to count exactly those executions and nothing else. This runs against
// a REAL Postgres because the whole mechanism IS a correlated EXISTS join plus a per-execution NULL check; a
// fake store would return whatever it was handed and prove nothing about the SQL.
//
// KILLING MUTATIONS (executed 2026-08-11 — each reverts an assertion below to red):
//   - change `NOT predicted OR NOT graded` to AND in axis_read.go: Bypassing collapses 2 -> 0. The OR is the
//     entire guardrail — EITHER skipped limb is drift, and an AND would only flag heals that skipped BOTH.
//   - drop the `OR NOT graded` limb: the unverifiable execution stops counting, Bypassing 2 -> 1.
//   - grade by a join to action_verdict (first-wins per action shape) instead of action_execution.verdict:
//     the ungraded RE-execution would inherit an earlier clean shape-verdict and escape — the exact stale-row
//     trap migration 0043 was written to defeat.
//   - drop the `executed_at >= $1` window: rows outside the audited period leak into the count.

import (
	"context"
	"testing"
	"time"
)

func TestLoopBypass_FlagsEachSkippedLimb(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	// A GLOBAL count needs a controlled table — the axis_a5 test wipes policy_graduation for exactly this
	// reason. No FK depends on action_execution, and every DB test self-seeds what it needs, so a full wipe is
	// safe and makes these counts exact rather than a fragile delta against unknown residue.
	wipe := func() {
		if _, err := p.Exec(ctx, `DELETE FROM action_execution`); err != nil {
			t.Fatalf("wipe action_execution: %v", err)
		}
		if _, err := p.Exec(ctx, `DELETE FROM infragraph_prediction`); err != nil {
			t.Fatalf("wipe infragraph_prediction: %v", err)
		}
	}
	wipe()
	defer wipe()

	seedExec := func(actionID string, verdict *string, unverifiable bool) {
		if _, err := p.Exec(ctx, `
			INSERT INTO action_execution (action_id, external_ref, verdict, unverifiable, target_host, site)
			VALUES ($1, $2, $3::verdict, $4, 'lb-host', 'nl')`,
			actionID, "lb-ref-"+actionID, verdict, unverifiable); err != nil {
			t.Fatalf("seed execution %s: %v", actionID, err)
		}
	}
	seedPrediction := func(actionID string) {
		if _, err := p.Exec(ctx, `
			INSERT INTO infragraph_prediction (plan_hash, action_id, target_host, prediction_hash, schema_version)
			VALUES ($1, $2, 'lb-host', 'lb-phash', 1)`,
			"lb-plan-"+actionID, actionID); err != nil {
			t.Fatalf("seed prediction %s: %v", actionID, err)
		}
	}
	match := "match"

	// (1) LOOP-COMPLIANT: committed a prediction AND core/verify graded it. Must NOT be flagged.
	seedExec("lb-compliant", &match, false)
	seedPrediction("lb-compliant")

	// (2) ACTED UN-PREDICTED: executed and graded, but no committed prediction. Flagged (NoPrediction).
	seedExec("lb-nopred", &match, false)

	// (3) UNGRADED: committed a prediction, but core/verify could not read the post-state (verdict NULL,
	//     unverifiable — TG-182 fail-closed). Flagged (NoVerdict), NOT excused by the prediction it did commit.
	seedExec("lb-noverdict", nil, true)
	seedPrediction("lb-noverdict")

	// A window that comfortably contains all three now()-stamped executions.
	got, err := NewAxisReadStore(p).LoopBypass(ctx, time.Now().UTC().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("LoopBypass: %v", err)
	}
	if got.Executed != 3 {
		t.Errorf("Executed = %d, want 3 (all three executions are in the window)", got.Executed)
	}
	if got.Bypassing != 2 {
		t.Errorf("Bypassing = %d, want 2 — the un-predicted AND the ungraded execution each skipped a limb; "+
			"the compliant one skipped neither. A 0 here means the OR guardrail was ANDed shut.", got.Bypassing)
	}
	if got.NoPrediction != 1 {
		t.Errorf("NoPrediction = %d, want 1 (only lb-nopred acted with no committed prediction)", got.NoPrediction)
	}
	if got.NoVerdict != 1 {
		t.Errorf("NoVerdict = %d, want 1 (only lb-noverdict executed without a core/verify grade; the prior "+
			"clean shape must NOT excuse this execution's null verdict — the 0043 stale-row trap)", got.NoVerdict)
	}
}

// TG-365 emptiness: a window with no executions must report a clean zero, never crash and never misreport.
// "Nothing to audit" is a distinct, honest state from "audited and found zero drift"; the renderer says so,
// and this pins that the store returns the all-zero the renderer keys on.
func TestLoopBypass_EmptyWindowIsCleanZero(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	// A window in the future contains no executions regardless of table residue — the emptiness is intrinsic,
	// not a side effect of a wipe.
	got, err := NewAxisReadStore(p).LoopBypass(ctx, time.Now().UTC().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("LoopBypass on empty window: %v", err)
	}
	if got.Executed != 0 || got.Bypassing != 0 || got.NoPrediction != 0 || got.NoVerdict != 0 {
		t.Errorf("an empty window must be all-zero (absent is not a pass), got %+v", got)
	}
}

// TG-448 — a legitimately prediction-less SEALED INVERSE must NOT false-flag as a loop-bypass. A manual
// rollback (TG-462) seals its compensating inverse with NO model prediction on purpose: the interceptor's
// STRUCTURE gate asserts the sealed action identity and a human approval authorizes the release, so the
// prediction gate is not on that path and no infragraph_prediction row exists. Before this fix such an
// executed inverse counted as NoPrediction (and Bypassing) — the guardrail firing on a compliant action.
//
// The seal is recorded QUERYABLY only as action_execution.inverts_action_id (the interceptor persists
// InvertsActionID but not the Gated flag), and TG-462's RollbackWorkflow is the SOLE inverse producer and
// always structure-gates — so inverts_action_id IS NOT NULL is the sound "structure-gated inverse" proxy.
//
// The exclusion must stay NARROW, which this pins with three seeds:
//   - a sealed inverse (inverts set, no prediction, graded)      → excused entirely: NOT NoPrediction, NOT Bypassing
//   - a genuine bypass (inverts NULL, no prediction, graded)     → STILL flagged: NoPrediction and Bypassing
//   - a sealed inverse that ran UNVERIFIED (inverts set, no       → STILL flagged on the VERIFY limb: NoVerdict and
//     prediction, verdict NULL)                                    Bypassing — the excuse is the prediction limb ONLY
//
// KILLING MUTATIONS (each reverts an assertion below to red):
//   - remove the exclusion (`NOT predicted AND NOT sealed_inverse` -> `NOT predicted`): the two sealed inverses
//     count as NoPrediction, NoPrediction 1 -> 3 and Bypassing 2 -> 3 — the false-positive this fix removes.
//   - broaden the exclusion to all rows (drop the inverts_action_id predicate so every un-predicted row is
//     excused): the genuine forward bypass escapes, NoPrediction 1 -> 0 — a real loop-bypass gone silent.
//   - leak the excuse into the verify limb (excuse an ungraded inverse from NoVerdict too): NoVerdict 1 -> 0,
//     an inverse that acted and could not be verified reads as loop-compliant.
func TestLoopBypass_SealedInverseExcusedFromPredictionLimbOnly(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	wipe := func() {
		if _, err := p.Exec(ctx, `DELETE FROM action_execution`); err != nil {
			t.Fatalf("wipe action_execution: %v", err)
		}
		if _, err := p.Exec(ctx, `DELETE FROM infragraph_prediction`); err != nil {
			t.Fatalf("wipe infragraph_prediction: %v", err)
		}
	}
	wipe()
	defer wipe()

	// seedExec appends one execution. invertsActionID names the forward action this row undoes; "" is a forward
	// action and is stored as NULL (the CHECK constraint rejects a blank-but-present value, exactly as the
	// production Record path does).
	seedExec := func(actionID string, verdict *string, unverifiable bool, invertsActionID string) {
		var inverts any // NULL for a forward action
		if invertsActionID != "" {
			inverts = invertsActionID
		}
		if _, err := p.Exec(ctx, `
			INSERT INTO action_execution (action_id, external_ref, verdict, unverifiable, target_host, site, inverts_action_id)
			VALUES ($1, $2, $3::verdict, $4, 'lb-host', 'nl', $5)`,
			actionID, "lb-ref-"+actionID, verdict, unverifiable, inverts); err != nil {
			t.Fatalf("seed execution %s: %v", actionID, err)
		}
	}
	match := "match"

	// (1) SEALED INVERSE, graded: a manual rollback that ran and was verified. No prediction is legitimate here.
	//     Must NOT be flagged on either limb.
	seedExec("lb-sealed-inverse", &match, false, "lb-forward-A")

	// (2) GENUINE BYPASS: a forward action with no prediction (inverts NULL). The guardrail MUST still catch it.
	seedExec("lb-genuine-bypass", &match, false, "")

	// (3) SEALED INVERSE, UNVERIFIED: the inverse ran but core/verify could not read the post-state (verdict NULL,
	//     unverifiable — TG-182 fail-closed). Excused from the PREDICTION limb, but the VERIFY limb must STILL
	//     fire: "we rolled back and could not prove it worked" is exactly what the guardrail exists to surface.
	seedExec("lb-inverse-unverified", nil, true, "lb-forward-B")

	got, err := NewAxisReadStore(p).LoopBypass(ctx, time.Now().UTC().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("LoopBypass: %v", err)
	}
	if got.Executed != 3 {
		t.Errorf("Executed = %d, want 3 (all three executions are in the window)", got.Executed)
	}
	if got.NoPrediction != 1 {
		t.Errorf("NoPrediction = %d, want 1 — ONLY the genuine forward bypass. A sealed inverse commits no "+
			"prediction by design (structure-gated, TG-462); counting it here is the TG-448 false-positive. A "+
			"value of 3 means the sealed-inverse exclusion was removed; a 0 means it was broadened to every "+
			"un-predicted row and a real loop-bypass went silent.", got.NoPrediction)
	}
	if got.NoVerdict != 1 {
		t.Errorf("NoVerdict = %d, want 1 — the unverified sealed inverse is STILL flagged; the exclusion is the "+
			"prediction limb ONLY. A 0 means the excuse leaked into the verify limb and an inverse that could "+
			"not be proven to work reads as loop-compliant.", got.NoVerdict)
	}
	if got.Bypassing != 2 {
		t.Errorf("Bypassing = %d, want 2 — the genuine forward bypass (no prediction) and the unverified inverse "+
			"(no verdict); the graded sealed inverse skipped NEITHER live limb and must not count.", got.Bypassing)
	}
}
