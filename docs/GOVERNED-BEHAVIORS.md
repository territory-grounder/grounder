# GOVERNED-BEHAVIORS.md — Territory Grounder

**The formally-specified behaviors, ported as executable governed behavior.**

This document is the behavioral constitution of Territory Grounder (TG). It restates the
predecessor's EARS-style specifications (spec/001–007) as product-grade, single-org multi-user
requirement lists, re-founded on the greenfield Go / Temporal / PostgreSQL+pgvector stack and
hardened by the security+quality audit overlay. Each requirement is a testable behavior that the
control-plane SHALL exhibit; INV-22 mandates that every one is exercised by an executable test that
drives the actual code path.

> **How to read this doc.** Behaviors are grouped by governed-behavior family (BEH-1…BEH-7). Every
> requirement keeps its predecessor id (`REQ-0NN`) as a stable anchor. Provenance is tagged inline:
> **[F]** foundation (spec/00x, the design TG inherits) · **[R]** product reframe (paradigm-rule N,
> multi-user/single-org/de-solo) · **[O]** audit overlay (INV-NN, threat-model hardening). Terminology is
> identical across the doc set: **ActionManifest**, the execution/automation classes
> (`never-auto` / `canary` / `staged` / `auto`), the three autonomy bands
> **AUTO / AUTO_NOTICE / POLL_PAUSE**, the **inviolable mechanical safety core**, the **module
> system**, **Temporal**, the **LiteLLM model-gateway**, and the **RBAC roles / approver graph**.
>
> Cross-references: the structural enforcement of these behaviors lives in
> [ARCHITECTURE.md](ARCHITECTURE.md) and the invariant catalogue; the never-auto floor and risk dials
> live in [CONSTITUTION.md](CONSTITUTION.md); the persistence contracts referenced here are defined in
> [DATA-MODEL.md](DATA-MODEL.md); machine-readable wire contracts (`openapi.yaml`, `asyncapi.yaml`)
> are generated per [ARCHITECTURE.md](ARCHITECTURE.md).

---

## Cross-cutting product & safety rules (apply to every requirement below)

These invert the predecessor's solo/single-operator assumptions and bind every behavior to the
mechanical safety core. They are preconditions on all REQ-* statements — not exceptions to them.

- **[R] Multi-user, single-org by default (paradigm-rule 1).** One deployment serves one organization
  with many operators, roles, and teams — there is no `tenant_id` and no cross-org row-level-security
  isolation. Every state, audit, prediction, suppression, and eval row referenced below is org-global.
  The correlation key generalizes from a bare `issue_id` to **`external_ref`** (unique within the org's
  own trackers). Authority is checked against the acting user/role; least-privilege identity —
  per-source HMAC secrets, per-agent scoped credentials/mTLS, credential-revoke-as-kill — keeps each
  adapter, credential, rollback command, retrieval query, and approval vote to its granted capability
  as defense-in-depth, not tenancy.
- **[R] Humans are roles, not a person (paradigm-rule 2).** Wherever the predecessor paged "the
  operator," TG routes to the org's configured **approver graph** — RBAC roles, on-call rotation /
  escalation policy, quorum, and fallback approver. Veto/approval authority is checked against the
  acting user/role.
- **[R] `notify_required`, not `sms_required` (paradigm-rule 3, reframe).** The classifier emits a
  channel-agnostic **`notify_required`** flag; SMS/Twilio is one reference notifier behind the
  notifier+approval module, never *the* channel.
- **[R] Controls are API/RBAC/config-driven and audited — never host-local sentinel files
  (paradigm-rule 4).** Every autonomy toggle below is an org-scoped, RBAC-gated feature-flag in the
  policy store, audited on change, and ships **DARK by default** with observe-before-live promotion.
  **There is no "byte-identical legacy" off-state** — this is greenfield; the off-state is simply the
  non-autonomous (read-only investigation) baseline. Predecessor REQ-005 (byte-identical legacy
  output) is **retired** and does not appear as a TG requirement.
- **[O] The inviolable mechanical safety core is non-configurable (paradigm-rule 8, INV-09/10).** No
  org policy, feature-flag, confidence score, or module setting lifts the mechanical NEVER-auto
  floor, the two-lane fail model (advisory OPEN / remediation CLOSED), commit-before-poll ordering, or
  the "acting agent has no write path to its own verdict" rule.
- **[O] One grammar, one parser, one action id (INV-06/07).** Model output is a JSON-schema-constrained
  **typed tool-call**, parsed exactly once into a typed `Proposal`; a single canonical
  **`action_id = SHA-256(canonicalJSON(Action))`** is threaded unchanged and asserted at every stage.
  No stage re-parses raw model text; a mutated Action yields a new id that invalidates prior
  authorization and re-enters the gate.
- **[O] Provenance-bound evidence (INV-11).** Any AUTO/high-confidence claim is admissible only if it
  cites at least one recent, successful, relevant **`ToolResult`** the orchestrator itself captured —
  never agent free-text or a bare code fence.

---

## BEH-1 — Three-band risk classification (from spec/001)

**Family owner:** the `RiskClassifier` — a typed, deterministic admission gate that writes exactly one
`session_risk_audit` row per classification. **[F]** spec/001 · **[O]** INV-09.

- **REQ-001 — [F] spec/001 · [R] paradigm-rule 2.**
  WHEN a session is admitted for a low-risk *or* reversible-and-prediction-eligible action that is
  **not** on an org-designated criticality-tier host and whose predicted blast-radius is below the
  org threshold, the classifier SHALL emit band **AUTO** and mark the proposal for `[AUTO-RESOLVE]`.
  *(The predecessor "P0 host" becomes an org criticality tier — paradigm-rule 9.)*

- **REQ-002 — [F] spec/001 · [R] paradigm-rule 3.**
  WHEN the action is reversible-mixed on an org-critical host *or* the predicted blast-radius is
  wide, the classifier SHALL emit band **AUTO_NOTICE**: proceed with `[AUTO-RESOLVE]` **and** set
  `notify_required = true` so the org on-call group receives an out-of-band veto notice in parallel.

- **REQ-003 — [F] spec/001 · [O] INV-09.**
  WHEN the action is high-risk, irreversible, unpredicted, a verification **deviation**, or a
  novel-incident class (`ood:novel-incident`), the classifier SHALL emit band **POLL_PAUSE**: mark the
  proposal `[POLL]`, hold on durable pause/resume state, and notify the approver graph — **never
  proceeding on timeout**.

- **REQ-004 — [F] spec/001 · [O] INV-09/INV-10 · [R] paradigm-rule 8.**
  The classifier SHALL apply the **inviolable mechanical NEVER-auto floor** as a non-configurable
  precondition that clamps the band to **POLL_PAUSE** regardless of confidence, org policy, or any
  flag, for any action in the irreversible class (`mkfs`, `dropdb`, `zpool`/`zfs destroy`,
  `terraform`/`tofu destroy`, `kubectl delete`/`drain`, credential-revoke, reboot/halt, config-file
  overwrite, code/repo destruction, criticality-tier reboot, real jailbreak). An **unrecognized
  mutation is never treated as safe by omission** (INV-09: unknown action-class ⇒ never-auto ceiling).

- **REQ-006 — [F] spec/001 · [O] INV-06/INV-09.**
  IF the model output is unparseable, non-manifest-expressible, or ambiguous, the classifier's **only**
  defined behavior SHALL be to fail closed to **POLL_PAUSE** (the `Band` enum zero-value is the
  most-restrictive band, so any error/panic/unmatched path is fail-closed by construction). Output is
  never routed through a looser fallback grammar (INV-06).

- **REQ-007 — [F] spec/001.**
  WHEN an incident class has **no learned prior** (no prediction-eligible history for this
  `(alert_rule, host)`), the classifier SHALL fail closed to **POLL_PAUSE**.

- **REQ-008 — [F] spec/001 · [O] INV-11.**
  WHEN the `silent_cognition_guard` policy is active, the classifier SHALL strip any `[AUTO-RESOLVE]`
  marker whose response lacks a bound post-state **evidence** block, downgrading the session to a poll.
  Under TG, "evidence" means one or more orchestrator-captured `ToolResult` IDs checked for
  provenance/recency/success/target-relevance — a bare fenced block is rejected (INV-11).

- **REQ-005 — RETIRED. [R] paradigm-rule 4/7.**
  The predecessor's "byte-identical legacy output while the autonomy sentinel is absent" requirement is
  **dropped**. Autonomy is an org-scoped, audited feature-flag; the off-state is the non-autonomous
  read-only baseline, not a legacy code path.

**Persistence contract (see DATA-MODEL.md).** Exactly one immutable `session_risk_audit` row per
classification, carrying `risk_level`, `band` (`AUTO`/`AUTO_NOTICE`/`POLL_PAUSE`),
`auto_approved`, `auto_proceed_on_timeout`, `notify_required`, `signals_json`, `operator_override`
(role/actor id, [R]), and the `plan_hash` that joins to the prediction gate. **[O] INV-07:** the audit
row also records the canonical `action_id`; **[O] INV-19:** the row is a **required** output of the
decision function (omitting a field is a Go type error) and is appended to the tamper-evident ledger.

**Band-aware audit invariant.** A standing check SHALL FAIL if any `auto_approved` row is outside
`{AUTO, AUTO_NOTICE}` or carries a floor signal (`irreversible:*`, `criticality:reboot`, `deviation`).
The overlay adds **action-binding** (INV-07): the `[AUTO-RESOLVE]` is valid only for the exact
`action_id`/`plan_hash` it was classified against.

---

## BEH-2 — Fail-closed prediction gate + mechanical verdict (from spec/002)

**Family owner:** the `PredictActivity` / `VerifyActivity` pair inside the remediation Temporal
workflow. This is the **remediation lane** — it fails **CLOSED**. **[F]** spec/002 · **[O]** INV-07/INV-10.

- **REQ-101 — [F] spec/002 · [O] INV-10.**
  BEFORE any approval poll starts, the orchestrator SHALL commit a `plan_hash`-keyed **machine
  consequence prediction** — computed OUTSIDE the LLM by the infragraph model — to the append-only
  prediction store. Temporal enforces `PredictActivity → ApprovalActivity → ExecuteActivity →
  VerifyActivity` ordering: a poll activity **cannot start** without a persisted prediction.

- **REQ-102 — [F] spec/002 · [O] INV-06/INV-07.**
  IF a proposal has no committed prediction, the gate SHALL **DENY** the poll (default-deny). Under the
  overlay, `BuildApprovalPoll` accepts only a **`GatedProposal`** — a type constructible *only* by the
  `PredictionGate` activity — so "poll without a prediction" is **uncompilable**, closing the H-02
  alternate-grammar bypass.

- **REQ-103 — [F] spec/002 · [O] INV-10.**
  AFTER execution, a **mechanical `match`/`partial`/`deviation` verdict** SHALL be written **only** by
  the deterministic verifier (`computeVerdict(pred, observed)`), which diffs observed alerts against
  the committed prediction. **The acting LLM has no write path to the verdict columns** — the
  prediction and verification tables grant no UPDATE/DELETE to the model or session role.

- **REQ-104 — [F] spec/002 · [O] paradigm-rule 8.**
  IF the verdict is **`deviation`** (observed reality diverges from the committed prediction — i.e.
  surprise), the session SHALL **never auto-resolve**, regardless of band or confidence. Deviation
  routes to POLL_PAUSE / human.

- **REQ-105 — [F] spec/002.**
  WHEN the prediction gate is in **analysis-only** mode (org config; the reframe of the
  predecessor `INFRAGRAPH_DISABLED=1`), the gate SHALL **record without gating** — it writes the
  prediction and shadow verdict for evaluation but does not block, keeping the advisory lane fail-OPEN.

- **REQ-102b — [O] INV-07 (overlay-added binding).**
  The committed prediction, the approval choice, the executed tool-calls, and the verdict SHALL all be
  bound to the **same immutable content-hashed `ActionManifest`**. Each stage re-derives and asserts
  `action_id`; a mismatch is a hard fail-closed abort. Any change to the Action mid-session yields a
  new `action_id` that invalidates prior prediction/approval and forces a child-workflow **re-gate**
  (closes H-03: "a prediction exists" ≠ "the prediction is for the executed action").

**Falsifiability (carried forward).** The prediction store retains **shuffled-graph negative-control**
columns (`control_tp`/`control_fp`) so the eval is falsifiable by construction; the degree-preserving
shuffled control is mandatory (INV-22 property tests assert it is present).

---

## BEH-3 — Per-incident auto-resolve + escalation requeue (from spec/003)

**Family owner:** the band-aware reconciler and the `escalation_queue` requeue lane. **[F]** spec/003.

- **REQ-201 — [F] spec/003 · [O] INV-11.**
  The system SHALL close an incident **only after confirming the alert condition actually cleared**,
  verified by an orchestrator-captured `ToolResult` / independent post-condition check — not by the
  agent asserting success.

- **REQ-202 — [F] spec/003.**
  WHEN a band-**AUTO** session drives a host back to health, the reconciler SHALL reconcile the
  recovered host and transition the ticket to Done via the org's ticket module.

- **REQ-203 — [F] spec/003.**
  On **every** close, the system SHALL record a `resolution_type` (`auto_resolved` / `human_resolved` /
  `escalated` / `deferred` …) on the append-only close-out record.

- **REQ-204 — [F] spec/003.**
  IF a session ends with **no terminal result** (crash, timeout, indeterminate), the system SHALL leave
  the incident **open** (transition to `To Verify`), never silently closed.

- **REQ-205 — [F] spec/003 · [R] paradigm-rule 1.**
  The system SHALL record every outcome as a **per-incident best-outcome** row so an alert storm cannot
  inflate auto-resolve denominators (count incidents, not events). Best-outcome rollups are
  **org-global**, aggregated per incident, not per event.

- **REQ-206 — [F] spec/003.**
  WHEN an approval poll goes unanswered and the session archives, the system SHALL schedule a **delayed
  re-check** row in `escalation_queue` (`attempts`, `status`, `eligible_at`).

- **REQ-207 — [F] spec/003 · [O] INV-01/INV-12.**
  WHEN a queued re-check fires, IF the alert condition is still active the system SHALL **re-escalate
  and page** the org approver graph; ELSE it SHALL defer to the autocloser. The requeue **re-enters
  the gated pipeline** as an authenticated internal Temporal signal (never a bare re-trigger), keyed by
  `session_id`.

- **REQ-208 — [F] spec/003 · [R] paradigm-rule 2.**
  AFTER the per-incident unanswered-poll cap is reached, the system SHALL **stand down to a human** —
  escalating to the org's fallback approver / next on-call tier rather than retrying autonomously.
  *(A growing POLL_PAUSE backlog of reversible work is defined as a policy failure, not a success —
  foundation principle, [F].)*

---

## BEH-4 — Governance auto-demote + judge-death detection (from spec/004)

**Family owner:** the governance-metrics worker (a Temporal Schedule) and the judge-liveness monitor.
**[F]** spec/004 · **[R]** paradigm-rules 1/5.

- **REQ-301 / REQ-302 — [F] spec/004.**
  WHEN a `(host, alert_rule)` tuple recurs **≥ 3× within 30 days** as a genuine
  repeat-offender, the system SHALL **auto-demote** that tuple to **analysis-only** (its suppression /
  auto-resolve eligibility is revoked; Tier-1 suppression escalates it instead) — a circuit-breaker
  implemented as metric + audit + expiry, **with no manual-review step**.

- **REQ-303 — [F] spec/004.**
  The auto-demote SHALL **exclude intentional known-transients** (rows tagged as expected/known-benign)
  so declared transient patterns are not mistaken for offenders.

- **REQ-304 — [F] spec/004 · [R] paradigm-rule 4.**
  The demotion SHALL **auto-expire after 30 days** with no manual review (reversible by construction).
  Demotion state is an org-scoped policy row, **not** a hardcoded prompt or a host-local flag.

- **REQ-305 — [F] spec/004 · [O] INV-15/INV-22.**
  The judge-liveness monitor SHALL compute the fraction of recently-ended sessions carrying a **real
  local judgment** using **ONLY tables the judge does not write** (judge-independent source of truth),
  so a dead judge cannot certify itself alive.

- **REQ-306 — [F] spec/004.**
  IF fewer than **50%** of more than 3 eligible recently-ended sessions carry a judgment, the monitor
  SHALL raise a **judge-death** warning (a monitored failure class), routed through the org's
  escalation module.

> **[R] Retention split (paradigm-rule 5).** The governance/demotion decisions and the judge-liveness
> facts land on the **immutable audit spine** (append-only, integrity-preserving archival). The raw
> judged transcripts and scores are **purgeable operational memory** governed by org TTL /
> right-to-erasure — the two stores are drawn explicitly and never conflated.

---

## BEH-5 — Tier-1 known-transient & scheduled-reboot suppression (from spec/005)

**Family owner:** the deterministic pre-model suppression chain (dedup → blast-radius fold →
scheduled-reboot phase SR → known-pattern → active-memory), run **in code before any model is spent**.
Every phase fails **OPEN**; critical/unknown **always** escalate. **[F]** spec/005 · **[O]** INV-20.

- **REQ-401 — [F] spec/005.**
  The system SHALL match known-transient patterns by **host-agnostic rule** (the pattern is keyed on
  `alert_rule`, not a specific hostname), scoped to the org's estate.

- **REQ-402 / REQ-403 — [F] spec/005 · [R] paradigm-rules 1/3 · [O] INV-20.**
  WHILE a declared **blast-radius suppression rule** is currently valid, the system SHALL activate the
  fold and post the child alert as a **notice with no session**. The predecessor's "open YouTrack
  control issue = the on-switch" generalizes to an **org-scoped suppression-policy record** managed
  via the ticket module; **[O] INV-20** requires it be a **temporally-bounded** registry row
  (`valid_from`, `valid_until`, `last_verified_at`) that applies only if `now() BETWEEN valid_from AND
  valid_until AND last_verified_at` is fresh.

- **REQ-404 — [F] spec/005 · [O] INV-20.**
  WHEN an alert is an **on-schedule reboot** on a host carrying a **live, un-expired, un-killed**
  registered schedule whose **strict DST-correct window** contains the alert time, the system SHALL
  suppress it in phase SR. The schedule is a bi-temporal registry row (`kind`, `cron`,
  `observing`/`live`, `kill_switch`, `valid_until`), **live-config-confirmed** by a scheduled verifier
  (INV-20).

- **REQ-406 — [F] spec/005.**
  AFTER a suppressed scheduled reboot, the system SHALL **two-phase verify**: if the boot was **not** a
  clean `systemd-reboot` (dirty boot), it SHALL **reopen the incident and page** the approver graph.

- **REQ-405 — [F] spec/005 · [O] INV-20.**
  IF a suppression match is **unconfirmed / expired / unverified / contradicted**, the system SHALL
  **fail OPEN** to standard escalation (the incident is investigated). Suppression knowledge is
  assembled from the registry at runtime and is **never hardcoded into a prompt** (INV-20).

- **REQ-407 — [F] spec/005.**
  The system SHALL **never suppress a critical-severity reboot** (or any critical/unknown alert),
  regardless of any matching schedule.

- **REQ-408 — [F] spec/005 · [O] INV-04.**
  The dedup stage SHALL **reject future-dated or negative-age entries** at the boundary (grammar-
  validated envelope; a malformed timestamp is rejected before use, not silently trusted).

**Self-learning bounds (carried forward, [F]/[R]/[O]).** Learned schedules register as `observing` and
are promoted to `live` only after **≥ 2 in-window boots** (observe-before-live). Every learned/promoted
rule carries the safety floor + `valid_until` auto-expiry + no-manual-review circuit-breaking (INV-20);
rules never self-promote on a single observation. Discovery/promotion writers are org-scoped.

---

## BEH-6 — Interface contracts (from spec/006)

**Family owner:** the single authenticated Go router and the generated wire contracts. **[F]** spec/006
· **[O]** INV-01/INV-15. See [ARCHITECTURE.md](ARCHITECTURE.md).

- **REQ-501 — [F] spec/006 · [O] INV-01.**
  The system SHALL accept **stats** and **session-replay** requests over HTTP **only** through the
  mandatory, non-bypassable auth middleware (mTLS client cert **or** per-source HMAC over the raw body
  with timestamp + nonce replay window). An unauthenticated request is rejected **before body-parse**;
  a route registered with `auth=none` **fails to register at boot**.
  **[O] threat-model hardening:** there is **no privileged resume-with-prompt primitive** — a
  "session-replay" re-engagement mints a **new** Temporal workflow that re-runs the full gate from
  zero, seeded only by an immutable read-only `ContextSnapshot`; it never resumes a mutating session
  with attacker-supplied input (closes H-01/P0-2).

- **REQ-504 — [F] spec/006.**
  WHEN a replay/stats lookup targets an **unknown** id, the system SHALL return **not-found** — an id
  with no matching row (enforced by NOT NULL FK) is indistinguishable from a missing one, so a probe
  for a non-existent id reveals nothing.

- **REQ-502 — [F] spec/006 · [R] paradigm-rule 3.**
  WHEN an **ingest module** (receiver) fires, the system SHALL publish a **`triage.requested`** event
  to the routing layer (the AsyncAPI-declared internal event). Every ingest
  adapter normalizes to one canonical shape and runs `dedup → flap → burst → correlate` **in code
  before** publishing.

- **REQ-503 — [F] spec/006 · [O] INV-19.**
  The system SHALL guarantee the **`session_risk_audit` persistence contract** — one structured,
  required decision record per classification (BEH-1), appended to the tamper-evident hash-chained
  governance ledger.

- **REQ-505 — [F] spec/006 · [O] INV-15/INV-16.**
  The system SHALL guarantee **`schema_version` stamping** on every governed row: each table is
  version-stamped by its writer against a canonical registry; readers **reject future versions**
  (`SchemaVersionError`) rather than silently mis-reading. The DDL, JSON Schema, validators, and
  human-facing counts are all **generated from one typed source per entity** (INV-15) — no hand-
  maintained parallel contract.

- **REQ-506 — [F] spec/006.**
  The system SHALL guarantee the **`discovered_scheduled_reboots`** persistence contract (BEH-5): the
  bi-temporal schedule registry with kill-switch + `valid_until`.

- **REQ-507 — [F] spec/006.**
  The system SHALL guarantee the **`escalation_queue`** persistence contract (BEH-3): the append-only,
  rate-capped requeue lane whose rows re-enter the gated pipeline.

- **REQ-501b — [O] INV-15 (overlay-added).**
  The machine-readable **`openapi.yaml` / `asyncapi.yaml` / JSON Schemas** SHALL be **generated** from
  the canonical Go/Postgres model, cover **100% of routed endpoints** with declared auth/error/
  idempotency schemas, and embed a **non-null `generated_at` + source hash + coverage scope**. CI fails
  on any hand-written count, uncovered path, drift, or missing provenance. The API is the single
  contract the `frontend/` console and all modules consume — no second contract.

---

## BEH-7 — Content-aware spec↔code lockstep (from spec/007)

**Family owner:** the lockstep manifest binding each safety-critical Go file to its owning EARS spec.
This is a **product safety control**, not merely build hygiene ([R] paradigm-rule 10). **[F]** spec/007
· **[O]** INV-22.

- **REQ-701 — [F] spec/007 · [R] paradigm-rule 10.**
  The system SHALL record a **content hash for every governed safety-critical file** — TG's Go
  implementations of the risk classifier, prediction gate, verifier, suppression chain, actuation
  interceptor, ledger, and the schema/migration set — bound to their owning specs (BEH-1…BEH-6) in a
  lockstep manifest.

- **REQ-702 — [F] spec/007 · [O] INV-22.**
  WHEN a governed file changes but its owning spec does not, the system SHALL **report spec drift** and
  fail CI. **[O] INV-22:** the lockstep manifest may **not** exclude governed files (the predecessor
  excluded 11 of 12) — governed code cannot be left out of the runnable, hash-verified set.

- **REQ-703 — [F] spec/007 · [R] paradigm-rule 4.**
  The system SHALL accept re-stamped hashes only via an **authorized, audited approval action**
  (RBAC-gated, recorded in the ledger) — the reframe of the predecessor "operator re-stamp," never a
  host-local edit.

- **REQ-704 — [F] spec/007.**
  The drift check SHALL compare only **semantic spec content** (comment-insensitive) so a cosmetic
  comment edit cannot clear genuine drift.

---

## BEH-8 — Operator-managed policy engine (graduated autonomy access control) (from spec/015)

**Family owner:** the `policy.Engine` — one operator-managed access-control layer that consolidates TG's
scattered actuation gates (binary mutation gate, op-class ceiling, unit allowlist, stateful floor,
hardcoded territory-ack, canary poll file) into four global modes and a graduated per-op-class ladder,
composing ABOVE the constitutional mechanical floor (spec/001, spec/013) and lifting none of it. **[R]**
paradigm-rule 4 · **[O]** INV-09/INV-21. New family (spec/015); its requirement block is **REQ-15xx**.

- **REQ-1500 — [R] paradigm-rule 4 · [O] INV-09.** Exactly four global modes — **Shadow** (read-only;
  log/suggest/rationale; engine off), **HITL** (every action → human approval; engine off), **Semi-auto**
  (engine decides auto/approve/deny; engine on), **Full-auto** (engine preset auto-all-except-floor;
  engine on) — of which exactly one is active.
- **REQ-1501 — [F] · [R] paradigm-rule 8.** The ingest → reason → rationale → propose → risk-classify
  pipeline is identical in all four modes; the mode governs ONLY the actuation branch.
- **REQ-1502 — [O] INV-19.** Mode transitions are authority-gated and appended to the tamper-evident
  ledger before taking effect; the fail-closed default is Shadow.
- **REQ-1503/1504 — [O] INV-06/INV-21/INV-09.** The evaluator is a FIXED, audited embedded OPA/Rego
  module (in-process, no sidecar); operator rules enter as ordered DATA (never Rego); a **deny-overrides**
  effect makes a matching deny win regardless of rule order.
- **REQ-1505/1506/1507/1508 — [F] · [O] INV-07/INV-11.** The rule model (`match` / `verdict` / `params` /
  `approve_by`), the `auto`/`approve`/`deny` trinary, param inheritance from a global-default rule, the
  `min_confidence` clamp, and the `rate_limit` governor.
- **REQ-1509/1510 — [O] INV-09 · [R] paradigm-rule 8.** Band composition: `respect` (default) emits the
  more-restrictive of {policy verdict, spec/001 risk band}; `force` overrides the band (double-warned)
  without lifting the constitutional floor.
- **REQ-1511/1512/1513 — [F] · [R] paradigm-rule 4.** The deny-floor is an EXECUTION floor, not a
  proposal floor (TG still reasons/suggests floor-class ops in every mode); loadable `conservative`
  (predecessor safe-exec argv denies + 30/min governor) and `bare` templates; removable with a warning,
  never blocked, and the constitutional floor still clamps beneath a removed template.
- **REQ-1514/1515 — [R] paradigm-rule 4 · [O] INV-10.** Per-op-class graduation promotes `approve` → `auto`
  after N verified clean runs and demotes on the first `deviation`; verify-on-auto (predict → execute →
  verify → breaker) runs even on `auto`.
- **REQ-1516 — [O] INV-12/INV-13 · [R] paradigm-rule 1.** `approve_by` principals resolve against BOTH a
  TG-local principal/group registry and a federated LDAP/OIDC provider; `/v1/vote` admits a vote only
  from a member of the pending decision's `approve_by` set.
- **REQ-1517 — [R] paradigm-rule 4.** The engine WARNS but does NOT block a permissive operator policy —
  a single allow-all is reachable behind a red double-confirmation and never refused. This is a
  deliberate departure from imposing a second hard floor above the constitution; it is safe because the
  constitutional mechanical floor (INV-09) is not one of the removable operator templates.
- **REQ-1518 — [O] INV-19.** Every policy decision is appended to the tamper-evident ledger as a required
  output of the evaluation function.
- **REQ-1519 — [R] paradigm-rule 4 · [O] INV-09.** Per-mode engine-toggle overrides warn (force-on in
  Shadow/HITL) or double-warn (disable in Semi/Full); an absent/unreadable mode fails closed to Shadow.
- **REQ-1520/1521 — [R] paradigm-rule 4/7 · [O] INV-09/INV-21.** The mode state machine is the SOLE
  mechanical actuation chokepoint: `TG_MUTATION_ENABLED`, the standalone console mutation toggle, AND the
  separate `core/safety.MutationGate` object are RETIRED and ABSORBED into it (one source of truth — no
  two states meaning "can actuate"). The mode absorbs the gate's behaviors (zero/unknown → Shadow;
  Semi/Full enable gated on the green preflight; breaker/`/halt` force Shadow). The absorption is a
  deliberate, audited safety-core refactor that re-expresses the INV-09/INV-21 gate obligations in
  mode-chokepoint terms; the independent defense-in-depth layers (never-auto deny-floor, host-side
  forced-command SSH key, per-action policy verdict) stay distinct.

---

## BEH-9 — Credential / Identity Engine (per-target credential resolution + unified sync) (from spec/016)

**Family owner:** the `credential.Engine` — the AUTHENTICATION sibling of the policy engine (spec/015 is
authorization). It resolves the identity TG uses to reach each target (SSH key / user / become / api token
/ port / connection scheme) from a native config-not-code store OR the estate's existing platforms, and
fails closed (an unresolved target is refused, never a default identity). Composes ABOVE the constitutional
mechanical floor (INV-09), which it never lifts. **[R]** paradigm-rule 4 · **[O]** INV-13/INV-09. New
family (spec/016); its requirement block is **REQ-16xx**.

- **REQ-1600/1601/1602 — [R] paradigm-rule 4 · [O] INV-09/INV-17.** A `Resolve(target) → CredentialBundle`
  entry point maps `host` / `host-glob` / `resource` / `group` / `device-class` to a required-field bundle
  (config-not-code, generalizing the `hostdiag` allowlist); an unresolved target is refused — never a
  default, global, or last-used identity (the resolver's zero value is refuse).
- **REQ-1603 — [O] INV-13.** Every credential value is a `core/config.SecretRef` reference resolved at
  runtime through the #27 sealed store (`env:` / `file:` / `store:` schemes + `RegisterStoreResolver`); no
  plaintext credential in config, the store, or any exportable artifact.
- **REQ-1604/1605 — [O] INV-21/INV-18.** Identity resolves AFTER a non-deny policy verdict and BEFORE
  execute (authN composes with authZ, two independent fail-closed layers); both engines key off ONE shared
  estate object-model, built once and referenced by both.
- **REQ-1606/1609/1610 — [O] INV-09.** Deterministic most-specific-wins rule precedence and operator-declared
  cross-source precedence; an equal-specificity or ambiguous conflict fails closed; the native store is the
  standalone fallback when nothing is synced.
- **REQ-1607/1608/1615 — [R] paradigm-rule 4 · [O] INV-05/INV-16/INV-19.** A `CredentialSource` interface
  with scheduled + on-demand `Sync()` performs a read-only, incremental, idempotent pull into the native
  store keyed by `(source_id, native_object_id)`; `last_synced_at` + drift recorded and surfaced.
- **REQ-1611/1612/1613/1614 — [O] INV-13/INV-05/INV-12.** Two identity planes kept distinct: the machine →
  host plane (AWX / Ansible / Semaphore / OpenBao / HashiCorp Vault + native) and the human → console
  approver plane (LDAP / OIDC, feeding spec/015 `approve_by`); each connector is a native Go client,
  distroless (no subprocess), read-only, fail-closed, grounded in vendor API docs; the `vault:` / `bao:`
  scheme fails closed on unreachable / denied / expired.
- **REQ-1616 — [O] INV-09/INV-21.** The AWX / Ansible / Semaphore effect channel is a Phase-1-safe read-only
  SENSOR; governed playbook / job-template actuation routes through the spec/015 mode chokepoint and the
  never-auto floor — a documented secondary capability, credential/identity resolution + sync being primary.
- **REQ-1617 — [O] INV-19/INV-17.** Every resolution and every sync run is appended to the tamper-evident
  ledger as a required output (non-secret metadata only); a source is admitted only when its connector
  adapter is compiled in and registered. TG-89 is a hard dependency of spec/015's interceptor integration
  (TG-98).
- **REQ-1618 — [R] paradigm-rule 4 · [O] INV-13/INV-19.** A first-class (Phase-1) console credential surface:
  sync-source config / test / schedule / "Sync now" with last-synced + drift; the per-target credential map
  (source + precedence + native fallback); a COVERAGE view ("can TG reach target X?") paired with the policy
  packet-tracer ("may TG act on target X?"); a shared object-groups editor consumed by both engines; and
  sealed write-only, ledger-audited secret edits (never echoed), rendering only real state, themed and
  responsive.

---

## Standing invariant benchmark (orchestration scorecard)

**[F]** foundation · **[O]** INV-22. A Temporal Schedule SHALL weekly **replay a fixed adversarial
synthetic incident stream** through the **isolated** classify→predict spine (an ephemeral Postgres,
**never** the live org DB — the live-DB-leak counter must stay 0) and assert:

- **I1 — Safety-composition:** every irreversible op lands `POLL_PAUSE`/high — **safety never composes
  away** (the mechanical floor holds under chained tool composition; NIST 4-axis, chains are the unit of
  risk).
- **I2 — Determinism:** identical input yields identical band/verdict.
- **I3 — Completeness:** every incident produces a terminal decision record.
- **I4 — Zero interaction-graph gaps:** no orphaned stage in the decision graph.

**[O] INV-22 hardening:** this synthetic scorecard is an **advisory low-weight smoke test only** and
can **never** be the deployment authority. Release is gated by **adversarial, boundary-complete**
executable tests (malicious / concurrent / replayed / delayed / partial-failure inputs) that drive the
**actual** code path and assert on observed state — at least one adversarial test per declared trust
boundary, plus a production-like canary. Source-string / schema-presence-only assertions are
prohibited for safety-critical behavior.

---

## Provenance map (quick reference)

| Behavior family | Predecessor spec | Reframe ([R]) | Overlay hardening ([O]) |
|---|---|---|---|
| BEH-1 3-band risk classification | spec/001 (REQ-001…008) | roles-not-operator; `notify_required`; org criticality tier; REQ-005 retired | INV-06 one-grammar; INV-07 action-binding; INV-09 fail-closed/unknown-class; INV-11 evidence |
| BEH-2 fail-closed prediction + verdict | spec/002 (REQ-101…105) | analysis-only is org config | INV-06/07 GatedProposal + ActionManifest; INV-10 verdict-by-code-only |
| BEH-3 per-incident auto-resolve + requeue | spec/003 (REQ-201…208) | org-global best-outcome rollups; role stand-down | INV-01/12 authenticated requeue signal; INV-11 confirmed-clear |
| BEH-4 auto-demote + judge-death | spec/004 (REQ-301…306) | org-scoped tuples; retention split (audit vs purgeable) | INV-15/22 judge-independent, generated metrics |
| BEH-5 tier-1 + scheduled-reboot suppression | spec/005 (REQ-401…408) | org suppression-policy record; org estate | INV-20 temporally-bounded live-verified registry; INV-04 boundary validation |
| BEH-6 interface contracts | spec/006 (REQ-501…507) | `triage.requested` org-scoped; correlation `external_ref` | INV-01 mandatory auth; INV-15 generated 100%-coverage contracts; no resume-with-prompt |
| BEH-7 spec↔code lockstep | spec/007 (REQ-701…704) | product safety control (not just build hygiene); RBAC re-stamp | INV-22 no governed-file exclusion |
| BEH-8 operator-managed policy engine | spec/015 (REQ-1500…1521) | four-mode graduated ladder; mode is sole actuation chokepoint; warn-don't-block; OPA rules-as-data; federated identity | INV-06/21 fixed-Rego deny-overrides; INV-09 constitutional floor beneath the engine; INV-10 verify-on-auto; INV-12/13 approve_by; INV-19 audit-every-decision |
| BEH-9 credential / identity engine | spec/016 (REQ-1600…1618) | per-target resolver (config-not-code); unified sync (import not hand-enter); two identity planes; native fallback; first-class console UX | INV-13 secret-as-reference/sealed; INV-09 fail-closed no-default-identity; INV-05 read-only re-read; INV-17 registered-only sources; INV-19 audit-every-resolution |

---

*This document specifies **behavior**. Its structural enforcement (typed router, `GatedProposal`,
`ActionManifest`, Temporal activity ordering, the module system, the LiteLLM model-gateway) is in
[ARCHITECTURE.md](ARCHITECTURE.md); the mechanical never-auto floor and risk dials are in
[CONSTITUTION.md](CONSTITUTION.md); persistence contracts are in [DATA-MODEL.md](DATA-MODEL.md); the
generated wire contracts are in [ARCHITECTURE.md](ARCHITECTURE.md). Where this document and the code
disagree, this document is intent and the code is the bug.*
