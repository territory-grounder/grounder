<!-- spec/002 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/002 — Fail-closed prediction gate + mechanical verdict

**Owning behavior family:** BEH-2 (see [`docs/GOVERNED-BEHAVIORS.md`](../../docs/GOVERNED-BEHAVIORS.md)).
**Constitution / invariants:** INV-06, INV-07, INV-10.
**Phase:** the `PredictActivity` / `VerifyActivity` pair lands in Phase 2; the content-hashed
`ActionManifest` binding they thread over is already built in `core/manifest` (Phase 0/1).
**Status:** Approved.

This is the **remediation lane** — it fails **CLOSED**. Before any approval poll opens, the orchestrator
commits a `plan_hash`-keyed machine consequence prediction computed outside the LLM by the infragraph
model; after execution a deterministic verifier writes the only `match`/`partial`/`deviation` verdict,
which the acting model can never author. A `deviation` (observed reality diverging from the committed
prediction) never auto-resolves. Every stage re-derives and asserts the same content-hashed `action_id`,
so "a prediction exists" can never be mistaken for "the prediction is for the executed action". This
document is the requirement source of record; the design is in `design.md`, the runnable acceptance
oracles are in `acceptance/`, and the engineering tasks are in `tasks.json`.

## Requirements

- **REQ-101** — [F] spec/002 · [O] INV-10.
  BEFORE any approval poll activity starts, the orchestrator SHALL commit a `plan_hash`-keyed machine
  consequence prediction — computed outside the LLM by the infragraph model — to the append-only
  prediction store, enforced by the Temporal activity ordering
  `PredictActivity → ApprovalActivity → ExecuteActivity → VerifyActivity` under which an approval poll
  activity cannot start without a persisted prediction. The committed prediction SHALL render to a
  single judge-readable summary (`verify.Prediction.Summary()`) that the offline eval and the live
  Runner both use, so falsifiable_prediction is scored over an identical string in both (TG-61).

- **REQ-102** — [F] spec/002 · [O] INV-06/INV-07.
  IF a proposal has no committed prediction, THEN the gate SHALL DENY the approval poll (default-deny),
  and `BuildApprovalPoll` accepts only a `GatedProposal` — a type constructible only by the
  `PredictionGate` activity — so a poll without a committed prediction is uncompilable, closing the H-02
  alternate-grammar bypass.

- **REQ-103** — [F] spec/002 · [O] INV-10.
  AFTER execution, the deterministic verifier `computeVerdict(pred, observed)` SHALL be the sole writer
  of the mechanical `match`/`partial`/`deviation` verdict, diffing observed alerts against the committed
  prediction, where the acting LLM has no write path to the verdict columns (the prediction and
  verification tables grant no UPDATE or DELETE to the model or session role).

- **REQ-103a** — [F] spec/002 · [O] INV-10 (Gulli ch19 rubric / ch4 structured critic).
  In the SAME single pass that decides the verdict, the deterministic verifier SHALL also return a typed
  `VerdictDetail` — the `match`/`partial`/`deviation` verdict PLUS the structured breakdown that produced it:
  the surprise hosts (in-scope, non-target observed hosts the prediction never named, each a `deviation`
  trigger) and the rule mismatches (observed alerts on a predicted host carrying a rule the prediction did
  not name, each a `partial` trigger). The bare `computeVerdict` verdict SHALL be derivable from that detail
  and byte-identical to the pre-existing verdict for every input (deviation dominates partial dominates
  match), so no verdict decision changes; verify-time callers consume the typed detail rather than
  recomputing the surprise/mismatch breakdown from the raw prediction and observation.

- **REQ-104** — [F] spec/002 · [O] paradigm-rule 8.
  IF the verdict is `deviation` — observed reality diverges from the committed prediction — THEN the
  session SHALL never auto-resolve regardless of band or confidence, and instead routes to POLL_PAUSE
  and the approver graph.

- **REQ-105** — [F] spec/002.
  WHILE the prediction gate is in analysis-only mode (an org-global RBAC-gated policy, the reframe of the predecessor
  `INFRAGRAPH_DISABLED=1`), the gate SHALL record the prediction and its shadow verdict for evaluation
  without blocking the approval, keeping the advisory lane fail-open.

- **REQ-102b** — [O] INV-07 (overlay-added binding).
  The committed prediction, the approval choice, the executed tool-calls, and the verdict SHALL all be
  bound to the same immutable content-hashed `ActionManifest`; each stage re-derives and asserts
  `action_id`, a mismatch is a fail-closed hard abort, and any mid-session change to the Action yields a
  new `action_id` that invalidates the prior prediction and approval and forces a child-workflow
  re-gate (closing H-03: "a prediction exists" is not "the prediction is for the executed action").

## Persistence contract

Every gated proposal writes one immutable `infragraph_prediction` row into the append-only prediction
store, stamped `schema_version`, carrying the `plan_hash`, the bound `action_id`
(INV-07), the predicted cascade, the prediction window, and the negative-control columns
`control_tp` / `control_fp`. Every executed action writes one immutable verdict row, authored only by
`computeVerdict`, carrying the `match`/`partial`/`deviation` result, the observed-versus-predicted diff,
and the same `action_id`. Both rows are chained into the tamper-evident governance ledger (INV-19). See
[`docs/DATA-MODEL.md`](../../docs/DATA-MODEL.md).

## Falsifiability contract

The prediction store SHALL retain a degree-preserving shuffled-graph negative control alongside every
real prediction (`control_tp` / `control_fp`), so the gate's predictive value is falsifiable by
construction: if real predictions do not beat the shuffled control, the eval fails. INV-22 property
tests assert the control columns are present and populated for every prediction row.
