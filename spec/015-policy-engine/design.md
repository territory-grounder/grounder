<!-- spec/015 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/015 — Design: operator-managed policy engine (graduated autonomy access control)

How the requirements in `requirements.md` are realized on the Go / Temporal / PostgreSQL stack. Where
this design and the code disagree, the code is the bug and this document is the intent. The engine
COMPOSES over the already-built controls (the mechanical safety core spec/001, the prediction gate
spec/002, the mechanical verdict, the actuation interceptor spec/013, the ledger spec/006); it adds an
operator-managed access-control layer and a graduated flip. It replaces none of the mechanical floors —
they run beneath it, defense-in-depth.

## Components

- **`policy.Engine`** (`core/policy/engine.go`) — the single entry point the actuation interceptor
  (spec/013) consults before it decides auto / approve / deny for a classified, gated action. `Decide`
  takes a typed `EvalInput` (the sealed `ActionManifest`, the spec/001 risk `Band`, the op-class,
  reversibility, host, territory, bound confidence, and the active `Mode`) and returns a required-field
  `PolicyDecision` (`Verdict`, `MatchedRuleID`, `ComposedBand`, `ApproveBy`, `Mode`). Producing a
  `PolicyDecision` with a missing field is a compile error, which is how the persistence contract
  (INV-19) is enforced at the type level (REQ-1518).
- **`policy.Mode`** (`core/policy/mode.go`) — the four-mode enum whose zero value is `ModeShadow`, so an
  absent or unreadable persisted mode fails closed to read-only Shadow (REQ-1519). Exactly one mode is
  active; `SetMode` gates the transition on an authenticated authority check and appends the ledger
  record before the new mode is observable (REQ-1500, REQ-1502). Shadow and HITL run with the engine
  off (Shadow suggests only; HITL routes every action to approval); Semi-auto and Full-auto run with the
  engine on. The mode governs ONLY the actuation branch — `pipeline_guard.go` asserts the ingest → reason
  → rationale → propose → risk-classify stages take the same code path in every mode (REQ-1501).
  **The mode IS the sole actuation chokepoint (REQ-1520/1521).** `TG_MUTATION_ENABLED`, the standalone
  console "Mutation OFF / read-only" toggle, AND the separate `core/safety.MutationGate` object are all
  RETIRED and ABSORBED into the mode state machine (paradigm-rule 7 — one source of truth; two states
  both meaning "can actuate" would be a sync bug surface). Everything the gate did lives in the mode now:
  the zero/unknown mode is Shadow (the fail-closed zero-value property), a transition into Semi-auto or
  Full-auto is gated on the spec/013 green preflight (what `EnableMutation` did), "may this action
  actuate?" is `mode ∈ {Semi-auto, Full-auto}` (what `gate.Enabled()` did), and a deviation-breaker trip
  or `/halt` forces `mode = Shadow` (what `gate.Disable()` did). This absorption is a deliberate,
  audited safety-core refactor (task T-015-13, REQ-1521), not a silent deletion: it re-expresses the
  INV-09 / INV-21 gate obligations in mode-chokepoint terms across the constitution, the breaker, the
  `/halt` handler, the boot preflight, spec/013, and `.lockstep.lock`. The genuinely independent
  defense-in-depth layers stay distinct control layers (NOT folded into the chokepoint): the never-auto
  deny-floor (deny-overrides, step 0) and the per-action policy verdict. Host-side hardening (a dedicated
  actuation user + a sudoers verb-allowlist) is an OPTIONAL operator backstop — TG uses one ordinary
  scoped actuation credential, never a per-command forced-command key (unscalable); the authoritative
  control is the policy engine.
- **Breaker recovery — the escalation re-arms the trip (REQ-1525).** The trip half is symmetric with a
  recovery half. The deviation breaker (`core/safety.MutationBreaker` over the durable, cross-process
  `mutation_breaker_state` row) forces Shadow on a trip, but that row is INDEPENDENT of the mode: restoring
  the mode does NOT clear it, so before this a single trip — even a false one — refused every actuation
  forever (`Tripped()` stays true, and the interceptor's REQ-1210 gate consults it beneath the mode). The
  recovery reuses the one governed way out of Shadow: `ModeController.Transition`, when it escalates INTO an
  actuating mode (`to.MayAutoActuate()`), calls an injected `BreakerRearmer` AFTER the transition is audited
  + activated. The worker binds the sole implementation (`breakerRearmer`) — the only process holding the
  armed breaker, its shared store, and the ledger — which appends `safety:breaker-rearm` (audit-before-effect)
  then calls `MutationBreaker.Rearm` → `breaker.Breaker.Reset` (force-close the row, reset the deviation
  counter). It is best-effort + fail-safe: a re-arm failure leaves the breaker OPEN (actuation stays halted)
  and never unwinds the recorded transition; a transition to Shadow/HITL or a red-preflight-refused escalation
  never re-arms; and the breaker NEVER self-heals (no automatic path — `Reset` is called only from this
  owner-gated site). Trip (breaker→Shadow) and recovery (escalation→breaker-closed) are thus both owner-gated
  and both ledgered.
- **`policy.Evaluator`** (`core/policy/eval.go` + `core/policy/rego/policy.rego`) — the OPA/Rego core.
  `policy.rego` is a FIXED, audited module compiled once via `github.com/open-policy-agent/opa/rego` and
  evaluated in-process (distroless-safe, no sidecar). The operator's rules enter ONLY as `input.rules`
  DATA — an ordered, readable ASA-style list — never as Rego source (REQ-1503). The module implements
  **deny-overrides** (REQ-1504): it collects every matching rule and returns `deny` if any match denies,
  else the highest-authority allow/approve, so a deny is order-independent and cannot be shadowed. The
  OPA decision-explanation output backs the console packet-tracer.
- **`policy.Rule`** (`core/policy/rule.go`, `core/policy/schema.go`) — the rule data model (REQ-1505):
  `match {op_class | argv_pattern | host | group | territory | reversible}`, `verdict {auto | approve |
  deny}`, `params {min_confidence, band_mode, rate_limit}`, `approve_by {user:* | group:*}`. `params`
  fields left unset inherit from the global-default rule (REQ-1507); `argv_pattern` matches the raw
  command string (the deny side), while `op_class` matches the semantic class (the allow side).
- **`policy.Bands`** (`core/policy/band.go`) — band composition (REQ-1509/1510). `respect` returns the
  more-restrictive of {policy verdict, spec/001 risk band}; `force` returns the policy verdict and stamps
  the double-warn flag. Neither can lift the constitutional floor: `safety.IsNeverAuto` /
  `safety.IsDestructiveOp` still clamp beneath the engine.
- **`policy.Floor`** (`core/policy/floor.go`, `core/policy/templates/*.json`) — the operator deny-floor
  (REQ-1511/1512/1513). The `conservative` template ports the predecessor `safe-exec.sh` argv
  deny-patterns plus the 30/min governor; `bare` carries no operator denies. Removal returns a
  `WarnRemoveFloor` requiring a double-confirmation and is never refused (REQ-1513). The deny is an
  execution floor: the pipeline still proposes floor-class actions with rationale in every mode; the deny
  bites only at auto-execute (REQ-1511).
- **`policy.Graduation`** (`core/policy/graduation.go`) — per-op-class promote/demote (REQ-1514). A class
  starts `approve`, promotes to `auto` after N consecutive verified `match` runs with no `deviation`, and
  demotes to `approve` on the first `deviation`. Promotion requires verify-on-auto to be wired
  (REQ-1515); the graduation counter reads ONLY the deterministic verifier's verdicts (spec/002,
  INV-10), never the acting model.
- **`policy.Identity`** (`core/policy/identity.go`, `core/policy/approve.go`, `core/policy/federated.go`)
  — principal resolution (REQ-1516). Local-first: a TG-local principal-and-group registry; federated:
  an optional LDAP/OIDC provider resolved behind the same interface (later phase). `/v1/vote` admits a
  vote ONLY when the voting principal is a member of the pending decision's `approve_by` set, checked
  against the decision's `decision_id` (INV-12) with the acting principal's authority (INV-13).

## Mode / verdict decision procedure (per candidate action)

The engine runs AFTER the spec/001 classifier has produced a `Band` and the spec/002 gate has committed
a prediction, and its verdict feeds the spec/013 interceptor's actuation branch. Ordered
most-restrictive-first so a permissive branch can never compose a floor away:

0. **Constitutional floor (beneath the engine).** `safety.IsNeverAuto(op) || !Reversible ||
   safety.IsDestructiveOp(op, op_class)` → the action can never resolve to `auto`; the engine may only
   raise scrutiny above this (REQ-1511, INV-09). This runs regardless of mode, template, or `band_mode`.
1. **Mode branch (REQ-1500/1501).** Shadow → suggest only, no actuation. HITL → `approve` for every
   action. Semi-auto / Full-auto → continue to rule evaluation. The mode never touches an earlier stage.
2. **Rego evaluation with deny-overrides (REQ-1503/1504).** The fixed module evaluates the operator rule
   data against the action; any matching `deny` wins. `argv_pattern` denies match the raw command; the
   `conservative` template's ports of the predecessor deny-list live here.
3. **Confidence + rate clamps (REQ-1507/1508).** Below `min_confidence` → clamp to `approve`; over the
   rule's `rate_limit` in the trailing minute → clamp to `approve`.
4. **Band composition (REQ-1509/1510).** `respect` → more-restrictive of {verdict, risk band}; `force` →
   policy verdict, double-warn stamped.
5. **Graduation adjustment (REQ-1514).** A class still in `approve` graduation state is not yet promoted
   to `auto`; a class that has met its clean-run bar evaluates at `auto`.
6. **Verify-on-auto (REQ-1515).** An `auto` verdict authorizes execution only through the spec/013
   predict → execute → verify → breaker chain; an unverifiable post-state refuses.
7. **Audit (REQ-1518).** One `policy_decision` row per evaluation, appended to the governance ledger.

## The consolidation (what this replaces)

| Scattered gate today | Folded into |
|---|---|
| binary mutation gate (spec/013 `MutationGate`) | the four-mode enum (REQ-1500) |
| op-class ceiling / unit allowlist | `op_class` match rules (REQ-1505) |
| stateful-workload floor | constitutional floor beneath the engine (unchanged, step 0) |
| hardcoded territory-ack (`cmd/worker/main.go:1342`) | `territory` match + `approve` verdict (REQ-1505) |
| canary poll file | graduation start-at-`approve` state (REQ-1514) |

The mechanical `safety` floors are NOT folded in — they stay at the classifier (spec/001) and the
actuation adapter (spec/013) as defense-in-depth beneath the engine.

## Persistence & audit

Every `Decide` appends one `policy_decision` row inside the same Temporal activity, stamped
`schema_version` and chained into the governance ledger (INV-19). Mode transitions, template loads,
floor removals, and graduation events each append their own immutable ledger record. The runtime DB role
holds no UPDATE/DELETE on these append-only tables (spec/006, migration `0016_policy_engine`).

## Out of scope

The classifier that bands an action is spec/001; the prediction gate and the verdict function are
spec/002; the single actuation chokepoint and the mutation keystone are spec/013; the ledger mechanics
and RBAC/auth surface are spec/006. This spec owns the operator-managed policy layer and the graduated
flip that compose over them.
