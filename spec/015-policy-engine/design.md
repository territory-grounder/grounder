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
- **`policy.Graduation`** (`core/policy/graduation.go`) — per-op-class promote/demote (REQ-1514, widened by
  spec/028 REQ-2804/2807/2808). A class starts `approve` and climbs **two** rungs; it demotes to `approve` —
  all the way to the bottom, never one rung — on the first verified `deviation`. Promotion requires
  verify-on-auto to be wired (REQ-1515); the graduation counter reads ONLY the deterministic verifier's
  verdicts (spec/002, INV-10), never the acting model.

  **The rungs.** `approve` ("asks first") → `auto_notice` ("acts and pages") after N consecutive verified
  `match` runs → `auto` ("acts silently") after a second, longer streak of `DefaultNoticeThreshold` runs
  counted on its own `notice_run_count`. The second streak is a separate counter rather than a reuse of
  `clean_run_count` because a demotion has to be unambiguous about which climb the count belonged to — a
  shared counter would leave "3" meaning either "3 of 5 toward acting" or "3 of 10 toward acting silently",
  and an operator reading the ladder could not tell which. The two bars differ in what they can establish:
  the first asks "does this op do what it claims?", which a handful of runs can answer; the second asks "is
  anyone still reading the pages?", and the only evidence for that is a long, boring stretch in which a
  watched autonomy produced nothing worth watching.

  **`auto_notice` permits the same VERDICT as `auto`.** At both rungs the class acts without a human vote —
  that is what "graduated" has always meant here. The notice is carried by the risk BAND instead
  (`NoticeFloor`, spec/028 REQ-2809), because a verdict decides *whether* the action happens and at this rung
  it does happen; what changes is who finds out. Encoding the notice as a verdict would silently turn "someone
  gets paged" into "the action does not occur".

  **The AUTO ceiling is structural (ADR-0016 decision 2).** The `auto_notice` → `auto` promotion additionally
  requires `opschema.IsEmbedded(op_class)` — membership in the embedded, lockstep-hashed registry, i.e. a code
  release. A class admitted through the runtime overlay may earn every run it makes and still stops at
  `auto_notice`: the difference is not how well it performed but which tamper domain grants it, and the rung
  where NO HUMAN WATCHES must live in the domain whose contents a runtime write cannot change. The predicate
  deliberately consults the EMBEDDED registry rather than `opschema.Lookup` (the composed view) — asking the
  composed view would let a ratification lift its own ceiling. A held class PINS its streak at the bar and
  reports `CeilingHeld` rather than resetting, so the console can offer the embed-export MR instead of showing
  an operator a class endlessly re-climbing a ladder it can never finish.

  **Per-class N may only ever RISE.** `WithPerClassThreshold` resolves the first bar from the
  `promote_threshold` a ratification stored (tier table: low-reversible ⇒ 5, everything else ⇒ 10). A resolved
  value at or below the ladder-wide threshold is IGNORED rather than honored — the Go mirror of
  `CHECK (promote_threshold >= 5)` on `opclass_ratified`, so the clamp still holds for a row predating the
  CHECK or a future non-DB resolver. An unpersisted promotion is refused at BOTH rungs
  (`ErrPromotionNotPersisted`): either would leave the class acting at a rung the durable record does not show
  it earned.

  **Rollout.** The `Level` constants are written explicitly rather than via `iota` — inserting a rung into an
  `iota` block silently changes what an integer MEANS, and the zero value must remain `approve` (fail closed)
  under any future insertion. The durable format is text, and `parseLevel`'s unknown→`approve` arm is the
  rolling-deploy contract: an old worker reading a new `auto_notice` row resolves it to `approve` and routes
  to a human vote, which is why the rung could ship by widening the CHECK (the 0040 precedent) rather than by
  a coordinated stop-the-world upgrade.
- **Vote admission** (REQ-1516) — a vote is admitted ONLY when the voting principal is a member of the
  pending decision's `approve_by` set, bound to the decision's sealed action (INV-12) with the acting
  principal's authority (INV-13). The LIVE mechanism (TG-254 → TG-488 B26/TG-463): `group:` entries are
  expanded to CONCRETE members at GATE time in the composition root over the synced credential human
  plane (`cmd/worker/approve_by_wiring.go`) and FROZEN into workflow history; admission is the pure
  `runner.VoterAdmitted` membership test (deterministic-replay-safe — no identity backend in workflow
  code); a chat-presented identity is resolved to its canonical login SURFACE-SIDE by the TG-463
  voter-alias normalizer (resolution only, never a wider set). The original actor-side
  `PrincipalResolver`/`MayApprove` lane (`core/policy/identity.go`/`approve.go`, `modules/policyident`)
  was RETIRED with zero production callers under the B26 ruling: live group resolution at vote time is
  unsound against the frozen-at-gate contract, and "LDAP federation is a resolution path, not a wider
  set" — the synced human plane IS the LDAP integration.

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
