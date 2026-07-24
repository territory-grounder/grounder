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

## Persistence & audit

Every `Classify` call appends one `session_risk_audit` row (INV-19) inside the same Temporal activity,
stamped `schema_version`, and the row is chained into the governance ledger. The audit
row records `action_id` so an `[AUTO-RESOLVE]` is bound to exactly the action it was classified against
(INV-07).

## Out of scope

The prediction that feeds step 2 is owned by spec/002 (prediction gate). Suppression that may prevent a
session from ever reaching the classifier is spec/005. Ledger chaining mechanics are spec/006.
