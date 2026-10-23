// Package proposal defines Territory Grounder's ONE proposal grammar: the single typed representation
// the model must emit, and the sole ParseProposal entry point that produces it.
//
// Provenance: [O] INV-06 (one grammar; unparseable/non-manifest-expressible output is rejected, never
// routed through a looser fallback), H-02 (the predecessor's crown-jewel bypass: a second
// "Which plan? - Plan X:" grammar that ran AFTER the gate), spec/002 REQ-102 · [F] confidence-first
// reasoning discipline.
//
// The load-bearing property: there is EXACTLY ONE grammar, defined here and imported by both the
// parser and the (future) prediction gate. `BuildApprovalPoll` (spec/002) will accept only a
// GatedProposal derived from this Proposal, so an approval poll for a second, looser grammar does not
// type-check. The model emits a JSON-schema-constrained tool-call, never markdown with a sentinel
// marker; the ApprovalChoice is parsed deterministically as DATA and NEVER trusted as authority — the
// classifier and gate decide, not the marker.
package proposal

import (
	"github.com/territory-grounder/grounder/core/manifest"
)

// Proposal is the typed, validated result of parsing one model tool-call. Every field is data; none is
// authority. Confidence is the parseable CONFIDENCE scalar the reasoning discipline uses for STOP
// thresholds (the gate/classifier act on it, the model does not self-authorize with it).
type Proposal struct {
	ExternalRef    string          // correlation key (ADR-0010)
	Action         manifest.Action // the proposed action, ready to seal into an ActionManifest
	ApprovalChoice string          // the model's stated choice — parsed as DATA, never trusted as authority
	Confidence     float64         // 0..1 CONFIDENCE scalar (STOP thresholds live in the agent loop)
	Rationale      string          // free-text rationale (data only)
	// Prediction is the model's committed FALSIFIABLE OUTCOME prediction (benchmark axis A2, docs/BENCHMARK-AXES):
	// the specific, mechanically-checkable observation it expects when the fix works — metric + expected value +
	// timeframe (e.g. "df / free rises above 10% within 5 min"). DATA only (never control flow); it is surfaced
	// into the committed prediction the judge scores, so falsifiable_prediction measures the model's OUTCOME
	// claim, not the action+blast-radius descriptor. Empty ⇒ the committed prediction falls back to the gate's
	// cascade (prior behaviour), so this never regresses.
	Prediction  string   // the committed falsifiable OUTCOME prediction (A2)
	EvidenceIDs []string // orchestrator-captured ToolResult ids the model cites (INV-11)
	// UndoSketch is the model's free-text sketch of how the proposed action would be reversed — the ONE
	// additive field of the open proposal plane (spec/026 REQ-2602). DATA only, never authority (INV-08):
	// it is screened (screen.Scrub) before persist, ledger, and console render, and it deliberately lives
	// HERE on the proposal record and never inside Action — manifest.Action is content-hashed into the
	// action identity (INV-07), so a field there would change every action id in the estate. Empty on a
	// registered-class proposal is normal; it is surfaced primarily for shadow (free-form) proposals where
	// no registered undo procedure exists yet (the dossier evidence spec/028 clusters on).
	UndoSketch string
	// Diagnosis is the typed, source-bound CLAIM this proposal rests on (TG-201). Empty when the model
	// returned none — additive, so a proposal without one is exactly the prior behaviour.
	Diagnosis Diagnosis
}
