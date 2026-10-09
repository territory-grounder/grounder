<!-- spec/030 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/030 — Transaction plans: multi-step repairs, approved once, all-or-nothing

**Owning behavior family:** extends BEH of spec/013 (the actuation chain) — a plan is a governed
SEQUENCE of the existing per-action behavior, never a new way to act.
**Constitution / invariants:** INV-03 (fixed argv, no shell), INV-07 (content-addressed action
identity — per STEP, unchanged), INV-08 (no model token as control flow: a plan is never
model-emitted), INV-12 (one approval decides — here, one approval for the PLAN), INV-19 (one ledger),
INV-21 (nothing actuates at Shadow).
**Phase:** Phase 2 (the TG-58 prerequisite; built now, inert until a recipe is declared AND mutation
is on).
**Status:** Draft — the governance model is OWNER-RULED (2026-08-22, recorded in the rulings log):
*"Plan approved once, but any step failing auto-reverts everything."* This spec turns that ruling into
requirements; the sub-decisions it forces are stated per-requirement rather than re-asked.

**The gap (TG-58 R19 / predecessor G7).** Certain repairs are inherently multi-step — drain the node,
restart the service, un-drain the node — and today TG cannot express them: the agent proposes ONE
action (INV-08, deliberate), the manifest binds ONE action, and every gate/vote/ledger record is
one-per-action. Executing such a repair as three independent sessions loses the property that matters:
if step 3 fails, steps 1–2 stand and the machine is left half-changed with nobody obligated to unwind
it. Temporal already runs the multi-step SESSION workflow durably; what is missing is the multi-step
ESTATE CHANGE with the bank-transfer property — all of it or none of it.

## Requirements

- **REQ-3001** — [F] Plans are orchestrator-composed, never model-emitted. A transaction plan SHALL
  come only from the compiled PlanRecipe registry (the opschema discipline: closed, validated at
  build, operator-authored). The agent's single proposed action MAY select a recipe — by op-class
  match, a pure lookup — but no model token SHALL name, order, extend or parameterize a plan's steps
  beyond the proposed action's own already-screened params (INV-08). Zero recipes declared ⇒ the whole
  lane is structurally inert.

- **REQ-3002** — [F] One approval for the whole plan, presented whole. A plan SHALL carry exactly ONE
  human approval (INV-12 lifted to the plan), and the poll SHALL present EVERY step — op, op-class,
  target, reversibility, and each step's compensation — before the vote. Approving a plan is approving
  its steps and its compensations; a step the poll did not show SHALL NOT execute. The vote binds a
  content-addressed plan_id derived from the ordered step tuples, so the thing approved is the thing
  executed (the INV-07 argument, one level up).

- **REQ-3003** — [F] Every step keeps the full per-action chain except the vote. Each step SHALL seal
  its own ActionManifest (per-step action_id, INV-07 unchanged) bound to the plan_id, SHALL be risk-
  classified individually, and SHALL execute through the UNCHANGED spec/013 interceptor chain — every
  gate, both admission leases, the mode chokepoint. The ONLY control the plan replaces is the per-step
  human vote (REQ-3002). Deny-overrides survives: any step whose classification lands harder than the
  plan's presented floor SHALL refuse the WHOLE plan before step 1 executes (fail closed — a plan is
  never partially admissible).

- **REQ-3004** — [F] Atomicity is the safety property: failure compensates automatically. When step N
  fails (refused by a gate, transport error, non-zero exit, verdict deviation), the plan workflow
  SHALL execute the compensations of steps N-1..1 in reverse order, automatically — the plan's one
  approval pre-authorized them (the owner's ruling; the commit-confirm armed-revert precedent). Each
  compensation is a first-class mutation through the FULL interceptor chain, derived from the step's
  registry rollback template (the RollbackWorkflow machinery). A recipe whose every step lacks a
  registered compensation SHALL be refused at BUILD time by registry validation — an uncompensatable
  step cannot join a plan.

- **REQ-3005** — [F] Compensation failure fails LOUD, never silent. If a compensation itself fails,
  the plan SHALL stop compensating, page (the commit-confirm revert-failed shape: page + mutation
  breaker trip), and record exactly which steps remain applied — a half-reverted machine with a human
  summoned and autonomy tripped is the honest floor; pretending atomicity where the estate refused it
  is the lie this spec must never tell.

- **REQ-3006** — [O] The ledger tells the whole story, per step, under one plan. Every transition —
  plan proposed, approved (the ONE vote), each step executed, each verdict, each compensation, the
  terminal (committed | reverted | revert-failed) — SHALL append to the governance ledger carrying
  BOTH the plan_id and the step's action_id, so the spine answers "what did this plan do" and "what
  touched this action" with the same rows. The session record carries the plan terminal.

- **REQ-3007** — [F] Ships inert three ways. The lane SHALL ship with zero declared recipes
  (REQ-3001: no recipe ⇒ no plan is ever composed and the single-action path is byte-identical), SHALL
  remain inert at Shadow like every mutation (INV-21), and SHALL refuse to compose any plan containing
  an op-class that is not itself graduated/admissible — a plan can never smuggle an op-class past the
  ladder that single actions must climb.
