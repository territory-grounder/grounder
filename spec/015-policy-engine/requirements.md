<!-- spec/015 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/015 — Operator-managed policy engine (graduated autonomy access control)

**Owning behavior family:** BEH-8 (see [`docs/GOVERNED-BEHAVIORS.md`](../../docs/GOVERNED-BEHAVIORS.md)).
**Constitution / invariants:** INV-06, INV-07, INV-09, INV-10, INV-11, INV-12, INV-13, INV-19, INV-21.
**Phase:** Phase 2 (governed autonomy — the graduated-flip layer above the mutation keystone spec/013).
**Status:** Draft.

The Policy Engine consolidates Territory Grounder's scattered actuation gates — the binary mutation
gate, the op-class ceiling, the unit allowlist, the stateful-workload floor, the hardcoded
territory-acknowledgement check, and the canary poll file — into ONE operator-managed access-control
layer, and turns the all-or-nothing mutation flip into a graduated ladder of four global modes. The
engine sits ABOVE the constitutional mechanical never-auto floor (spec/001 REQ-004, spec/013 REQ-1203,
INV-09): the engine can raise scrutiny but SHALL NOT lift that mechanical floor, which is enforced
beneath the engine at the classifier and the actuation adapter regardless of any policy. This document
is the requirement source of record; the design is in `design.md`, the runnable acceptance oracles are
in `acceptance/`, and the engineering tasks are in `tasks.json`.

> **Two distinct floors — do not conflate them.** (1) The **constitutional mechanical floor** (INV-09)
> is non-configurable, non-removable, and enforced beneath this engine; no policy, template, mode, or
> flag in this spec lifts it. (2) The **operator policy deny-floor** (the `conservative` template) is the
> engine-layer deny-list the operator manages; it is removable with warnings. Removing (2) never removes
> (1). The "warn, don't block" product invariant (REQ-1517) governs only the operator's own engine-layer
> policy, not the constitutional floor.

## Requirements

- **REQ-1500** — [R] paradigm-rule 4 · [O] INV-09.
  The engine SHALL expose exactly four global modes — **Shadow** (read-only; log, suggest, and record
  rationale; engine off), **HITL** (every candidate action routes to a human approval; engine off),
  **Semi-auto** (the engine decides auto / approve / deny per action; engine on), and **Full-auto** (the
  engine applies a preset that auto-approves every action except the floor; engine on) — of which exactly
  one SHALL be active at any instant.

- **REQ-1501** — [F] · [R] paradigm-rule 8.
  The investigation pipeline — ingest, reason, rationale, propose, risk-classify — SHALL be identical
  across all four modes; the active mode SHALL govern ONLY the actuation branch (whether a classified
  proposal is auto-executed, routed to approval, or denied) and SHALL NOT alter any earlier stage.

- **REQ-1502** — [O] INV-19.
  WHEN the active mode is changed, the engine SHALL gate the transition behind an authenticated,
  authority-checked operator action and SHALL append one immutable mode-transition record — prior mode,
  new mode, actor id, and reason — to the tamper-evident governance ledger before the new mode takes
  effect.

- **REQ-1503** — [O] INV-06 · [O] INV-21.
  The engine SHALL evaluate policy through an embedded Open Policy Agent Rego module invoked in-process
  via the Go SDK (no sidecar), WHERE the Rego module is a FIXED, audited evaluator that operators SHALL
  NOT edit, and the operator-managed policy SHALL be supplied to it as ordered rule DATA — an operator
  SHALL never author Rego.

- **REQ-1504** — [O] INV-09.
  The Rego evaluator SHALL apply a **deny-overrides** effect: WHEN any rule in the supplied data matches
  with verdict `deny`, the decision SHALL be `deny` regardless of rule order or any matching `auto` /
  `approve` rule, so a deny cannot be shadowed by rule ordering.

- **REQ-1505** — [F] · [O] INV-07.
  Each policy rule SHALL be a data record carrying a `match` selector over `{op_class` (semantic, the
  allow side) `| argv_pattern` (raw command string, the deny side) `| host | group | territory |
  reversible}`, a `verdict` of `auto | approve | deny`, a `params` object, and an `approve_by` list of
  `user:` / `group:` principals.

- **REQ-1506** — [R] paradigm-rule 2.
  The engine SHALL resolve every evaluated action to exactly one of the three verdicts `auto`,
  `approve`, or `deny`, WHERE `auto` authorizes execution under verify-on-auto (REQ-1515), `approve`
  routes the action to the `approve_by` principals for a human vote, and `deny` refuses execution and
  records the refusal.

- **REQ-1507** — [F] · [O] INV-11.
  A rule's `params` SHALL inherit each unset field from the global-default rule, and the engine SHALL
  clamp any action whose bound confidence is below the resolved `min_confidence` (default `0.60`) to at
  most an `approve` verdict — a below-threshold action SHALL NOT resolve to `auto`.

- **REQ-1508** — [F].
  WHILE a rule declares a `rate_limit` of N executions per minute, the engine SHALL count auto-executions
  matching that rule within the trailing minute and SHALL clamp the (N+1)th matching action in that
  window to at most an `approve` verdict.

- **REQ-1509** — [O] INV-09.
  WHILE a rule's `band_mode` is `respect` (the default), the engine SHALL emit the more-restrictive of
  {the policy verdict, the action's own risk band from spec/001 (`AUTO` / `AUTO_NOTICE` / `POLL_PAUSE`)},
  so a `POLL_PAUSE` risk band SHALL veto a permissive `auto` policy verdict.

- **REQ-1510** — [R] paradigm-rule 8.
  WHERE a rule's `band_mode` is `force`, the engine SHALL apply the policy verdict even when it is less
  restrictive than the action's risk band, and SHALL record a double-confirmation warning on the
  transition that sets `force` — the constitutional mechanical floor (INV-09) SHALL remain in force
  beneath the override.

- **REQ-1511** — [O] INV-08 · [O] INV-09.
  The engine deny-floor SHALL be an EXECUTION floor, not a proposal floor: in every mode the pipeline
  SHALL still reason about and suggest a floor-class action (including an irreversible one) with its
  rationale, and the deny SHALL take effect ONLY at the point an action would auto-execute under
  Semi-auto or Full-auto.

- **REQ-1512** — [F] · [R] paradigm-rule 4.
  The engine SHALL provide two loadable policy templates — `conservative` (the predecessor safe-exec
  guardrail: its argv deny-patterns plus a 30-executions-per-minute governor) and `bare` (no
  operator-layer denies) — and SHALL load the selected template as ordered rule data at the operator's
  request.

- **REQ-1513** — [R] paradigm-rule 4.
  WHEN an operator removes the `conservative` deny template or loads an allow-all policy, the engine
  SHALL require a distinct double-confirmation and SHALL permit the change; the engine SHALL NOT block a
  permissive operator policy, and the constitutional mechanical floor (INV-09) SHALL continue to clamp
  floor-class ops beneath the removed template.

- **REQ-1514** — [R] paradigm-rule 4.
  WHILE graduation is enabled for an op-class, the engine SHALL start that class at verdict `approve`,
  SHALL promote it to `auto` after N consecutive verified `match` runs with no `deviation` (default N
  configured per class), and SHALL demote it back to `approve` on the first `deviation` verdict.

- **REQ-1515** — [O] INV-10.
  WHEN the engine resolves an action to `auto`, the actuation SHALL still run the full
  predict → execute → verify → breaker sequence (spec/013), and the engine SHALL NOT authorize an `auto`
  execution whose post-state cannot be verified.

- **REQ-1516** — [O] INV-12 · [O] INV-13 · [R] paradigm-rule 1.
  The engine SHALL resolve `approve_by` principals against BOTH a TG-local principal-and-group registry
  and, WHERE a federated LDAP or OIDC provider is configured, that provider, and SHALL admit an approval
  vote on `/v1/vote` ONLY WHEN the voting principal is a member of the pending decision's `approve_by`
  set — a vote from a non-member SHALL be rejected.

- **REQ-1517** — [R] paradigm-rule 4.
  The engine SHALL warn on a permissive or self-guardrail-removing policy change but SHALL NOT block the
  operator from applying it — a single allow-all policy SHALL be reachable behind a red double-confirmation
  and SHALL never be refused by the engine.

- **REQ-1518** — [O] INV-19.
  The engine SHALL append every policy decision — the matched rule id, the resolved verdict, the
  band-composition result, the bound `action_id`, and the actor — to the tamper-evident hash-chained
  governance ledger as a required output of the evaluation function, so no policy decision executes
  without a persisted audit record.

- **REQ-1519** — [R] paradigm-rule 4 · [O] INV-09.
  WHERE an administrator overrides the per-mode engine default, the engine SHALL record a warning WHEN
  forcing the engine on in Shadow or HITL and a double-confirmation warning WHEN disabling the engine in
  Semi-auto or Full-auto, and SHALL default fail-closed to Shadow WHEN the persisted mode is absent or
  unreadable.

- **REQ-1520** — [R] paradigm-rule 4/7 · [O] INV-09/INV-21.
  The mode state machine SHALL be the SOLE mechanical actuation chokepoint — the `TG_MUTATION_ENABLED`
  environment knob, the standalone console "Mutation OFF / read-only" toggle, AND the separate
  `core/safety.MutationGate` object SHALL be RETIRED and absorbed into it, so exactly one state answers
  "may this action actuate?" and no two states meaning the same thing require synchronization. The mode
  SHALL absorb every behavior the gate held: a default or unknown mode SHALL be Shadow (the fail-closed
  zero-value property lives in the mode); a transition into Semi-auto or Full-auto SHALL be gated on the
  green preflight (spec/013 REQ-1206); "may this action actuate?" SHALL be true only WHILE the mode is
  Semi-auto or Full-auto; and a deviation-breaker trip or a `/halt` kill-switch SHALL force the mode to
  Shadow.

- **REQ-1521** — [O] INV-09/INV-21 · [R] paradigm-rule 7.
  The absorption of the `MutationGate` into the mode chokepoint SHALL be a deliberate, audited safety-core
  refactor — never a silent deletion — that re-expresses the INV-09 / INV-21 mutation-gate obligations in
  terms of the mode chokepoint across the constitution, the deviation breaker, the `/halt` kill-switch,
  the boot preflight, spec/013, and the `.lockstep.lock` manifest, and SHALL preserve every fail-closed
  property the gate guaranteed (default-off, preflight-gated enable, breaker/halt force-off). The
  genuinely independent defense-in-depth layers — the never-auto deny-floor (deny-overrides, REQ-1504)
  and the per-action policy verdict (REQ-1506) — SHALL remain distinct control layers, not folded into
  the chokepoint. Host-side hardening (a dedicated actuation user + a sudoers verb-allowlist) MAY be
  applied by an operator as an OPTIONAL independent backstop, but SHALL NOT be required by TG and SHALL
  NOT take the unscalable form of a distinct forced-command SSH key per command — the actuation path
  uses one ordinary scoped credential, and the authoritative control is the policy engine itself.

- **REQ-1522** — [O] INV-09 · [O] INV-19.
  The engine SHALL harden the policy decision surface for audit integrity and fail-closed evaluation.
  Every `PolicyDecision` SHALL carry the loaded rule-bundle version — a deterministic content-derived
  fingerprint of the evaluated rules-as-data — into its non-secret audit projection AND into the
  serialized detail of the persisted governance-ledger record, so the audit trail names exactly which
  rule set authorized or refused the action, WITHOUT a new database column or migration. Every
  `PolicyDecision` SHALL surface the FULL bounded, typed list of matched rules — each matched rule id
  paired with its declared verdict — preserving the deny-overrides outcome unchanged while making the
  decision explainable. WHEN the fixed Rego evaluator returns an error rather than a resolved verdict,
  the engine SHALL fail closed to `deny` (refuse the action) — never to a permissive verdict and never
  to `auto` — and SHALL surface the error.

- **REQ-1523** — [O] INV-09 · [F] out-of-box curated defaults (owner target: Semi=curated, Full=allow-all).
  ON a FRESH deployment (no operator ruleset persisted) the worker SHALL SEED a curated Semi-auto default
  baseline as loadable rules-as-data — a document granting `auto` ONLY to the reversible curated op-class
  family (restart-service, reload-service, restart-container) — so a fresh estate is capable of the common,
  reversible recoveries rather than granting `approve` to everything. Each curated `auto` rule SHALL still
  require `reversible: true` in its match, and a matched op still traverses novelty · band · mode · floor and
  the effect-leaf unit/container allowlist — so a curated-auto class actuates ONLY on an operator-allowlisted
  target, never estate-wide. The seed
  SHALL be ABSENT-ONLY and idempotent: it SHALL run ONLY when the ruleset store reports absent, SHALL NEVER
  overwrite an operator (or already-seeded) document, and a corrupt/other load error SHALL fail closed to
  the empty ruleset WITHOUT seeding over it. A curated `auto` rule alone SHALL NOT actuate: the worker SHALL,
  on the SAME fresh deploy only, seed the earned-graduation ladder to `auto` for exactly the curated
  reversible classes the default document names (absent-only per class — NEVER downgrading or overwriting an
  earned or operator-tuned class), and SHALL NOT touch the ladder when an operator ruleset already exists.
  The curated `auto` rule SHALL set `min_confidence: 0` EXPLICITLY (keeping the confidence gate off, matching
  the proven canary): an unset value inherits the 0.60 `EffectiveParams` fallback and would clamp the curated
  `auto` to `approve` on an unset/low bound confidence (an inert seed), and the settled calibration decision
  defers gating autonomy on the uncalibrated confidence scalar — so curated autonomy rests on the reliable
  gates (graduation · novelty · band · mode · floor), never confidence.
  The seed SHALL write autonomy defaults ONLY — it SHALL NEVER lift the mode: mutation stays Shadow by
  default (REQ-1520), so a seeded class actuates ONLY once an operator deliberately escalates the mode, and
  every non-curated op-class still earns autonomy from zero through the ladder. A seed write failure SHALL be
  tolerated (the class stays at `approve` — fail-closed), never fatal to boot.

- **REQ-1524** — [R] paradigm-rule 4 · [O] INV-09 · [F] configurable modes, Shadow default (owner target).
  WHERE a deployer declares an initial mode via deploy-time config on a FRESH deployment, the worker SHALL
  SEED that mode ONCE, ABSENT-ONLY: it SHALL apply the configured mode ONLY WHEN no mode has ever been
  persisted, and SHALL NEVER override an operator-set or previously-seeded mode (so it is a no-op on an
  established estate). The seed SHALL be fail-closed to Shadow: an unset/invalid/Shadow configured value, a
  nil store, or any store/ledger error SHALL leave the mode at its Shadow default (REQ-1519). Seeding INTO an
  actuating mode (Semi-auto/Full-auto) SHALL still require the green preflight (REQ-1520) exactly as a
  runtime transition does — a red/absent preflight SHALL refuse the seed and stay Shadow — and the seed SHALL
  be appended to the governance ledger BEFORE it takes effect (REQ-1502). The deployer's control of the
  config is the authority (no runtime RBAC check), and the mode chokepoint (may-actuate = mode ∧ green
  preflight), the never-auto floor, graduation, novelty, and band gates SHALL still govern every actuation,
  so a seeded actuating mode is exactly as safe as an operator flipping to it at runtime. A refused seed
  SHALL be logged and tolerated, never fatal to boot.

- **REQ-1525** — [O] INV-09/INV-21 · [R] paradigm-rule 4/7 · [F] recoverable deviation breaker.
  A deviation-breaker trip forces the mode to Shadow (REQ-1520), but the breaker is a durable, cross-process
  row INDEPENDENT of the mode: restoring the mode does NOT by itself clear the trip, so absent a recovery
  path a SINGLE trip — including a FALSE one — leaves the breaker open and every actuation refuses FOREVER,
  even after an operator restores an actuating mode. To close this gap WITHOUT weakening the fail-closed
  default, an owner-gated transition INTO an actuating mode (Semi-auto/Full-auto) — the deliberate "resume
  actuation" decision, already gated on authority (REQ-1502) and the green preflight (REQ-1520/REQ-1206) —
  SHALL RE-ARM (close) the deviation breaker. The re-arm SHALL be recorded to the governance ledger
  (`safety:breaker-rearm`, INV-19) and SHALL run only AFTER the transition is audited and activated; it
  SHALL be best-effort and fail-safe — a re-arm failure SHALL leave the breaker OPEN (actuation stays
  halted, never half-enabled) and SHALL NOT unwind the recorded transition. The breaker SHALL NEVER re-arm
  automatically (no self-heal): the ONLY re-arm path is this owner-gated escalation, and a transition to a
  non-actuating mode (Shadow/HITL) or a refused escalation SHALL NOT clear it. This makes the trip
  (breaker→Shadow) and the recovery (escalation→breaker-closed) symmetric, both owner-gated and both
  ledgered, so a breaker trip is RECOVERABLE rather than a permanent estate-wide actuation kill.

## Persistence contract

The engine writes exactly one immutable `policy_decision` row per evaluation, carrying the matched
`rule_id`, the resolved `verdict`, the `band_mode` and composed band, `min_confidence` used, the bound
`action_id` and `plan_hash` (INV-07), the acting principal, and the active `mode`. Mode transitions,
template loads, floor removals, and graduation promote/demote events each write one immutable
governance-ledger record. Every row is a required output of its decision function — omitting a field is
a Go type error — and is appended to the tamper-evident governance ledger (INV-19). See
[`docs/DATA-MODEL.md`](../../docs/DATA-MODEL.md).

## Policy-composition invariant

A standing check SHALL FAIL if any `policy_decision` row resolves to `auto` while carrying a
constitutional-floor signal (`irreversible:*`, `criticality:reboot`, `deviation`), if any `auto` row
lacks a bound verification observer (REQ-1515), or if a mode-transition record is absent from the ledger
for an observed mode change (REQ-1502). The deny-overrides property (REQ-1504) SHALL hold under any
permutation of the supplied rule data.
