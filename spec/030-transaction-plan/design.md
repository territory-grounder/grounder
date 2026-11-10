# spec/030 — design

## Shape: a saga over the existing chain, not a new authority

Temporal is the vehicle (the owner's own framing, and correct): a `TransactionPlanWorkflow` child owns
ordering, durability and compensation, exactly as `CommitConfirmWorkflow` owns the armed revert today.
Every estate-touching step goes through the UNCHANGED spec/013 interceptor — the plan adds sequence and
the bank-transfer property, never a second way to act.

## Components

- `core/plan` — `PlanRecipe` (name, trigger op-class, ordered `[]PlanStep{OpClass, ParamMap}`) + the
  compiled registry (`recipes()`, empty at ship; the modules/catalog + core/pack discipline). BUILD-time
  validation refuses: a step whose op-class lacks a registry rollback template (REQ-3004), an unknown
  op-class, a recipe over the step ceiling (compiled, small), duplicate names.
- Plan identity — `plan_id = hash(ordered step tuples)` (the manifest.Action.ID discipline, one level
  up). The vote binds it; the workflow re-derives and refuses on mismatch.
- `core/db` — `transaction_plan` row (plan_id, recipe, session ref, state machine: proposed → approved
  → executing → committed | reverted | revert-failed; plane: both — the commit_confirm projection precedent, the workflow history is the authority) + per-step rows binding
  step action_ids to the plan_id. Append-only state transitions.
- `temporal/runner` — recipe lookup at the propose terminal (pure, keyed on the proposal's op-class);
  the plan poll (REQ-3002: every step + compensation rendered into the SAME vote surface the single
  action uses today, one decision); `TransactionPlanWorkflow`: classify ALL steps first (deny-overrides
  whole-plan refusal), then execute in order via the existing ExecuteActivity, compensating N-1..1 on
  failure via the SealRollbackExecuteActivity machinery; page + trip on compensation failure
  (REQ-3005, the commit-confirm revert-failed shape).
- Ledger — `plan:proposed / plan:approved / plan:step-executed / plan:compensated / plan:committed /
  plan:reverted / plan:revert-failed`, each entry carrying plan_id + the step's action_id (REQ-3006).

## What deliberately does NOT change

The agent still proposes ONE action; a recipe match widens what the ORCHESTRATOR offers the human,
never what the model may say (INV-08). Per-step manifests, per-step classification, every interceptor
gate, the graduation ladder, and the mode chokepoint are untouched. A single-action session that
matches no recipe is byte-identical to today.

## Sequencing (tasks)

T-030-1 registry + validation → T-030-2 plan identity + durable rows → T-030-3 the workflow (classify-
all, execute-in-order, compensate, page-on-compensation-failure) → T-030-4 the plan poll + one-vote
binding → T-030-5 ledger/session-record shape → T-030-6 first real recipe (owner-chosen) + live drill
(drill deferred with all drills). Ships inert until T-030-6 declares a recipe.
