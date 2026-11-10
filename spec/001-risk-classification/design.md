<!-- spec/001 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/001 — Design: three-band risk classification

How the requirements in `requirements.md` are realized on the Go / Temporal / PostgreSQL stack. Where
this design and the code disagree, the code is the bug and this document is the intent.

## Component

`core/risk.Classifier` (Phase 2) is a pure function of a typed, already-validated input:

```
Classify(ctx, GatedInput) -> Decision
```

- **`GatedInput`** carries the typed `Proposal` (one grammar, parsed once — INV-06), the committed
  `plan_hash`-keyed machine prediction (spec/002), the organization criticality tier for the target host, the
  reversibility/op-class of the action, and the evidence set. It is constructible only downstream of
  ingest validation (spec/006) and the prediction gate (spec/002), so the classifier can never see raw
  model text.
- **`Decision`** is a required-field struct: `Band`, `RiskLevel`, `AutoApproved`, `NotifyRequired`,
  `AutoProceedOnTimeout` (always false), `Signals`, `ActionID`, `PlanHash`. Producing a `Decision` with
  a missing field is a compile error, which is how the persistence contract (INV-19) is enforced at the
  type level rather than by a runtime check.

## The mechanical safety core (already implemented — Phase 0)

The fail-closed primitives the classifier composes over live in `core/safety` and are the GREEN
acceptance anchors for this spec today:

- **`safety.Band`** — the zero value is `BandPollPause`, and `Band.String()` maps every out-of-range
  value to `"POLL_PAUSE"`. This is the mechanical realization of REQ-006/REQ-007: any error, panic, or
  unmatched path yields the most-restrictive band without the classifier having to remember to. [O] INV-09.
- **`safety.neverAutoFloor` (unexported) / `safety.IsNeverAuto(opClass)`** — the non-configurable set that
  clamps REQ-004. The classifier calls `IsNeverAuto` first; a hit forces POLL_PAUSE before any confidence,
  band, or organization policy is consulted. An op-class absent from the floor set is not thereby "safe" —
  the classifier's default for an unrecognized mutation is also the never-auto ceiling (REQ-004). The floor
  map is UNEXPORTED and reachable only through `IsNeverAuto` / `NeverAutoClasses` (which returns a fresh
  copy) so it is immutable-by-construction — no package can delete or add an entry at runtime to lift a
  floor op during a live canary (Phase-2 readiness hardening; the behavior REQ-004 requires is unchanged).
- **`safety.FailLane`** — the classifier runs in `LaneRemediation` (fails CLOSED). The zero value is
  `LaneRemediation` on purpose, so an unspecified lane is treated as the mutation lane.

## Decision procedure (Phase 2)

0. A detected prompt-injection / jailbreak in the untrusted input (`GatedInput.Jailbreak`, set by the
   `core/screen` pure-regex detector) → **POLL_PAUSE**, evaluated FIRST — an injected instruction may be
   steering everything downstream, so it forces the human circuit-breaker before any other reasoning.

   The `persona-shift` category enumerates instruction-override phrasings as a MATRIX — verb ×
   determiner × adjective × noun (ignore/disregard/forget/override/bypass/discard/skip ×
   previous/prior/earlier/above/original/preceding × instructions/context/messages/prompts/rules/
   directives/guidelines/directions/orders/constraints) — rather than as a list of observed phrases
   (TG-329). It was three hand-written phrasings, and the gap that exposed the difference was ordinary
   rather than exotic: `"Ignore previous instructions …"` was caught while `"disregard all prior rules"`
   passed through untouched. Because a clean row and an unrecognised row are indistinguishable at every
   call site — both return zero matches and carry no `screened:` tag — a miss reads as *screened clean*,
   and one gap is a gap in every untrusted-text path at once (lessons, AWX runbook ingest, host output).

   The nouns are deliberately instruction-like only. Ordinary operational text — "ignore the previous
   alert", "disregard earlier errors in the log", "override the default timeout to 30s" — MUST pass
   through byte-identical: a screen that neutralizes real host output corrupts the corpus in the name of
   protecting it, and every marker in a legitimate log line is a fact the agent can no longer read. Both
   directions are pinned by oracles in `core/screen`.

   The same `core/screen` gate carries the secret/PII redaction pass (REQ-010, `screen.Redact`, called
   by `screen.Scrub`): a leaked credential embedded in an untrusted body is deterministically replaced
   with a `[REDACTED:<kind>]` marker before the text reaches the model or any log/ledger write, so a
   proven leak class (a live NetBox token once landed in a predecessor plaintext log) cannot reach the
   seed or the governance ledger. Redaction is deliberately NOT a `Detect`/`IsJailbreak` signal — a
   stripped credential is a hygiene failure, not a jailbreak — so it neutralizes-and-flags on the same
   channel as an injection but never, by itself, clamps the band to POLL_PAUSE.
1. `IsNeverAuto(op)` or an unknown mutation class → **POLL_PAUSE** (REQ-004). No later step can lift this.
   A mutating action on a **stateful workload** (`safety.IsStatefulWorkload` — DB/queue/store/statefulset) is
   likewise clamped to POLL_PAUSE even when reversible: a restart/scale during sync or quorum can lose data.
   A purely read-only op on such a target is exempt. And a **server-derived destructive op**
   (`safety.IsDestructiveOp` over the ACTUAL command, not the model's declared op_class) clamps to POLL_PAUSE
   — a proposal claiming `restart-service` whose op is `dropdb prod` cannot hide its mutation ("a plan cannot
   hide a mutation"). The destructive-op pattern matches the predecessor's `k8s-delete-stateful` spelling in
   full: a `kubectl delete` of a stateful/structural object floors on the aliases AND the long forms
   (`pvc | persistentvolumeclaim | pv | persistentvolume\w* | namespace | ns | secret`) — matching only the
   `pv`/`pvc` aliases let `kubectl delete persistentvolumeclaim …` (data loss) slip to AUTO (PORT-FIDELITY-AUDIT).
   It likewise catches a Proxmox `qm|pct destroy` (permanent guest deletion) or `qm|pct reset` (hard
   power-cycle) — the predecessor's `qm reset`/`qm destroy` floor — a helm teardown
   (`helm uninstall|delete|rollback`, which removes a release and its PVCs), and `kubectl apply --prune`
   (which deletes any resource absent from the manifest), so an under-declared destroy/reset/teardown is
   server-derived destructive and polled. Finally, a **restart/reload of the platform's OWN control-plane service**
   (`safety.IsRestartClass` over the op + a config-declared self-protected set) clamps to POLL_PAUSE: the
   mission lane runs inside a session and auto-restarting the platform mid-session can orphan the running
   reconcile — a non-bypassable veto (PORT-FIDELITY-AUDIT P2-21, the predecessor's `_SELF_PROTECTED_RESTART_RE`).
1a. **Canary pin** — a `(host, op_class)` on the deployment-declared canary allowlist (`core/risk.CanaryPins`,
   set by the activity, config-not-code) → **POLL_PAUSE** (REQ-009). Placed after the inviolable floors (which
   record the more fundamental reason) but before the auto-eligible branches, so it can raise an
   otherwise-AUTO staged-canary action to a human vote — never lower one. Inert when unconfigured.
2. No committed prediction for `(alert_rule, host)`, or a `deviation` verdict, or
   `ood:novel-incident`, or a **high-risk alert category** (`maintenance` / `security-incident` /
   `deployment`, via `safety.HighRiskCategory`) → **POLL_PAUSE** (REQ-003, REQ-007). The category clamp is
   safe-direction only: it can raise review, never lower a band, and an unknown/empty category adds no clamp
   (the mechanical floor still governs it). This restores the predecessor's category-high-risk band default.
   The `ood:novel-incident` signal keys on the **incident subject** (`env.Host`, the alerted device) with
   the LLM-expressed action target as a legacy fallback key (REQ-003): the runner passes both into
   `novelIncident(subjectHost, actionTarget, rule)`, which de-novels on a precedent under EITHER key. This
   is what makes a de-novel transfer across proposals — the writeback records the subject, so the next
   occurrence is non-novel however its proposal expresses the target. Keying only on the action target (the
   pre-fix behaviour) made de-novel fail to transfer when the target alternated between the guest and its
   hypervisor for the same fault (TG-124).
3. `silent_cognition_guard` active and evidence absent/unbound → strip `[AUTO-RESOLVE]`, downgrade to a
   poll (REQ-008).
4. Reversible-mixed on a criticality-tier host, or wide predicted blast-radius → **AUTO_NOTICE** with
   `notify_required = true` (REQ-002).
5. Low-risk or reversible-and-prediction-eligible, below the configured blast-radius threshold, not on a
   criticality-tier host → **AUTO** (REQ-001).
6. Any parse/ambiguity/error at any step → **POLL_PAUSE** (REQ-006), via the `Band` zero value.

Steps are ordered most-restrictive-first so the floor can never be composed away by a later permissive
branch (the safety-composition invariant, GOVERNED-BEHAVIORS I1).

### The declared band floor — one seam, two producers (spec/028 REQ-2809, spec/026 REQ-2611)

`GatedInput` carries `BandFloor` / `BandFloorApplies` / `BandFloorReason`, and `Classify` composes the
declared floor over whatever steps 1–6 computed, **in the safe direction only**: a floor may raise the
approval bar and may never lower it. `safety.Band` is ordered most-restrictive-first
(`BandPollPause`(0) < `BandAutoNotice` < `BandAuto`), so "raise the bar" is the numeric minimum — the same
ordering property that makes the zero value fail closed.

Two producers deliberately share one field. The graduation ladder declares an **AUTO_NOTICE** floor for a
class sitting at the `auto_notice` rung (spec/028 REQ-2809 — it earned the right to act, not the right to act
unobserved); the actor-evidence policy (`proposal.FloorFor`) declares a **POLL_PAUSE** floor for
authored-action evidence (spec/026 REQ-2611 — a human already touched this, so ask). Giving each its own
bespoke field would put band adjustment in two places, and a second place bands can be adjusted is a second
place they can be adjusted *downward*.

Four properties, each with a RED-proven oracle in `core/risk/band_floor_test.go`:

- **The floor is applied as a WRAPPER over the whole procedure, not inside a branch.** Steps 1–6 return early
  from a dozen places; a clamp copied into each is one that will be missed by the thirteenth. Wrapping makes
  "no path escapes the floor" a property of the control flow rather than of everyone remembering.
- **A mechanical gate outranks a declared floor.** The floor composes *after* classification, so where a
  constitutional gate already polled, ITS `poll_reason` survives. An operator reading the audit row at 3am
  must see the strongest true reason — "irreversible-or-never-auto-floor", not "a policy floor applied".
- **`BandFloorApplies` is required because `safety.Band`'s zero value is the strictest band.** An unset floor
  field without the flag would clamp every action in the estate to a human poll. The explicit flag mirrors
  `proposal.FloorFor`'s own `(Band, bool)` signature so "no floor declared" survives the trip through the
  struct instead of decaying into "floor everything".
- **An AUTO_NOTICE floor preserves `AutoApproved`/`AutoResolve`; an unrecognised floor value falls to the
  `default` arm and polls.** Lowering the former would silently convert "act and page" into "do not act" — a
  different, unrequested refusal. The `default` arm (rather than an explicit `BandPollPause` case) is what
  makes a floor value the code does not understand fail closed.

## Persistence & audit

Every `Classify` call appends one `session_risk_audit` row (INV-19) inside the same Temporal activity,
stamped `schema_version`, and the row is chained into the governance ledger. The audit
row records `action_id` so an `[AUTO-RESOLVE]` is bound to exactly the action it was classified against
(INV-07).

## Out of scope

The prediction that feeds step 2 is owned by spec/002 (prediction gate). Suppression that may prevent a
session from ever reaching the classifier is spec/005. Ledger chaining mechanics are spec/006.

## Terminology note (2026-08-11, TG-112)

`core/safety`'s refusal error keeps its historical identifier `ErrMutationDisabled` (matched at 15 call
sites) but its MESSAGE now speaks the 4-mode model — "actuation refused — the mode does not permit it
(may_actuate=false)". The mutation-binary vocabulary is retired repo-wide (the owner ruling on TG-112);
`deploy/mutation_terminology_retired_test.go` is the closed-enumeration guard for the retirement. No
classification behavior changed.

## Destructive derivation: action vs prose (TG-641, owner-ruled 2026-11-06)

The server-side destructive derivation the classifier trusts over the model's declared op_class
(`safety.IsDestructiveOp`, "a plan cannot hide a mutation") reads two inputs of different kinds. The ACTION (op +
params, the command that would run) keeps the full destructive-op pattern. The model's RATIONALE is a sentence: there
`safety.IsDestructiveProse` applies the same pattern with exactly two segments changed. The bare host-power words
(`shutdown|halt|poweroff`) and the bare `revoke` count only in command shape (`shutdown -h now`, `systemctl poweroff`,
`qm shutdown 101`, `vault token revoke`). The prose pattern is derived from the action pattern's text, a shape change
panics at init, and a test proves the two differ only in those segments. Live trigger: a start-guest held four times in
the G3 soak because its rationale said the eventlog showed "no evidence of a planned shutdown". Bare `poweroff`
stays destructive in prose (a command token, not an English word), and SQL `REVOKE … FROM` and legacy `vault revoke`
count as revoke commands. The derived value is pure classify-activity input, so the workflow needs no version guard.

## Amendment 2026-11-07 — a still-open prior HOLD polls the re-detection (TG-667, deep-review F5)

`GatedInput` gains `PriorHoldOpen` / `PriorHoldDetail`, and the classifier gains one rule, placed after the
actor-attribution branches and before every auto-eligible branch: WHEN a STICKY hold decided inside the
re-detection window is still open for the incident's (host, rule), the action polls with reason
`prior-hold-still-open` and the audit row carries `prior_hold=<reason>@<ref> by <who> ... until <expiry>`.
Sticky reasons are the facts about the ESTATE or the ACTOR that a re-detection cannot refresh on its own —
`actor-attributed-authorized`, `actor-attribution-escalate`, `actor-attributed-suspicious`,
`stateful-workload-mutation`, `server-derived-destructive-op`, `irreversible-or-never-auto-floor`,
`self-protected-control-plane-restart`, an operator's `human:deny`, and a prior `prior-hold-still-open` (a
renewal). Reasons about the PROPOSAL (prose naming another host, no committed prediction) or about TG's own
first-look inputs (novelty, an ungraduated class, a canary pin) are not sticky and are re-judged fresh. The
rule only ever RAISES review: a deployment without the hold store reads `PriorHoldOpen=false` everywhere and
bands byte-identically. The hold memory itself (`hold_decision`, migration 0127; `core/holds`) and the
autonomous fallback approver's behaviour on it are specified in spec/012 (REQ-1127 amendment of the same
date). Why this exists: the actor-attribution lookback is 30 minutes, so a person's deliberate stop that TG
correctly held at minute 5 was eligible to be healed when the same alert re-fired at minute 40.

## Step 1b: the registry's reversibility declaration outranks the proposal's claim (2026-11-08, TG-685)

`Reversible` on the classifier input is the proposal's own claim, written by the model. The op-class registry now
declares `reversible: true|false` for every embedded class:
- `disk-grow` and `k8s-set-replicas` are false. A grown disk cannot be shrunk safely, and a scale-down's revert
  re-creates pods rather than restoring them.
- The start, restart, reload and guest-lifecycle classes are true.

An overlay class that declares nothing reads as irreversible.

Step 1b polls a registry-irreversible class with `irreversible-op-class` immediately after the never-auto floor. The
autonomous fallback approver (spec/012 REQ-1127) clears only a canary pin, an un-graduated class and novelty, so the
counterfactual still polls and the approver HOLDs. That reason is sticky (TG-667).

This closes the path that released `disk-grow` twice as `op-class-not-graduated`, on 2026-09-30 and 2026-10-09. The
AWX leaf refused both, so no effect occurred and no credit or rung was touched.
