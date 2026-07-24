<!-- spec/017 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/017 — Actuation Regime Engine (regime-aware effect lanes, AWX-job first)

**Owning behavior family:** — (no narrative `BEH-N`; this is a Phase-2 effect-channel family, like spec/013).
**Constitution / invariants:** INV-02, INV-05, INV-07, INV-09, INV-10, INV-11, INV-13, INV-17, INV-19, INV-21.
**Phase:** Phase 2 (governed actuation — the mutating lanes stay OFF until the owner-present flip; the read-only sensor + knowledge lanes are Phase-1-safe).
**Status:** Draft.

The Actuation Regime Engine is the **Act** stage of TG's autonomy loop: instead of mutating a target
directly, TG applies a change through a **regime-aware effect lane** selected per the target's management
regime — native-SSH (built, spec/013), **AWX-job** (the first new lane, the buildable slice of this spec),
GitOps-MR, k8s-declarative, and API. A heterogeneous estate is not one regime: a host managed by hand takes
raw SSH, a host owned by AWX takes an idempotent, reviewed, self-auditing **job template**, a cluster under
GitOps takes a merge request, never a direct write. Mutating an AWX-managed or GitOps-managed target
directly causes drift, revert, and split-brain; the regime engine makes mutation-ON both useful AND safe
across the estate by routing each change down the lane its target's regime sanctions.

This engine sits ON the already-built platform and NEVER replaces it. The policy engine (spec/015,
`Engine.Decide → PolicyDecision`) answers *may TG act?* (authZ); the credential engine (spec/016,
`Resolve → Bundle`) answers *with what identity?* (authN); the actuation interceptor (spec/013,
`core/actuate/interceptor.go`) is the wired chain (admission → never-auto floor → policy authorize →
credential authenticate → mode chokepoint → execute → verify); the mode chokepoint (`core/safety`,
`MayActuate = mode ∈ {Semi,Full} ∧ preflight`) is the sole actuation keystone. **Every lane of this engine
is an effect leaf that plugs INTO that interceptor — it authorizes nothing, authenticates nothing, and
lifts no floor of its own; it only selects and drives the effect channel beneath the same gates.** This
document is the requirement source of record; the design is in `design.md`, the runnable acceptance oracles
are in `acceptance/`, and the engineering tasks are in `tasks.json`.

> **A lane is a channel, not a permission.** Selecting the AWX-job lane for a target grants no new
> authority: the constitutional mechanical never-auto floor (INV-09), the policy verdict (spec/015), the
> per-target credential resolution (spec/016), and the mode chokepoint (`MayActuate`) all run BENEATH lane
> selection, unchanged. A target for which any of those refuses is not actuatable through any lane. Lane
> selection changes *how* an authorized, authenticated, mode-permitted change is applied — never *whether*.

> **The async gap is a first-class safety concern (from the GitOps-MR research).** An AWX job runs
> ASYNC — the launch is a prediction, not an observed effect. An actuation whose effect is not
> synchronously observable is NOT verified at launch; it is verified only when its deferred-verify channel
> polls the job to a terminal outcome and compares that outcome against the prediction. Until then it is
> pending-verification and counts as no clean run for graduation. A job launched but never verified fails
> closed for trust, never open.

## Requirements

- **REQ-1700** — [R] paradigm-rule 8 · [O] INV-21.
  The engine SHALL resolve each target to exactly one management regime through operator-declared
  configuration data (config-not-code) — one of `native-ssh`, `awx-job`, `gitops-mr`, `k8s-declarative`, `proxmox`, or
  `api` — and SHALL select the effect lane bound to that regime, exposing selection as a typed
  `SelectLane(target) → Lane` entry point the actuation path consumes in place of a hardcoded direct-SSH
  effect.

- **REQ-1701** — [O] INV-09.
  IF a target resolves to no known regime, THEN the engine SHALL resolve to the operator-declared default
  lane (`native-ssh`) WHERE the operator has declared one and SHALL refuse otherwise, and IF a target
  matches more than one regime, THEN the engine SHALL fail closed and refuse rather than select a lane
  arbitrarily — one regime per target, and ambiguity fails closed.

- **REQ-1702** — [O] INV-21 · [O] INV-02.
  Each effect lane SHALL be an effect leaf reachable ONLY through the spec/013 actuation interceptor chain
  (admission → never-auto floor → policy authorize → credential authenticate → mode chokepoint → execute →
  verify), and SHALL NOT expose a path that bypasses the never-auto floor, the policy verdict, the
  credential resolution, or the mode chokepoint; the lane's actuator field SHALL be unexported so the only
  way to reach its effect is the interceptor's `Do`.

- **REQ-1703** — [O] INV-17.
  The engine SHALL key regime and lane resolution off the SAME `host` / `host-glob` / `group` /
  `device-class` and inventory primitives that the policy engine (spec/015 REQ-1505) and the credential
  engine (spec/016 REQ-1605) match on — the one estate object-model built once and referenced by all three —
  and SHALL NOT define a second inventory grammar.

- **REQ-1704** — [O] INV-06 · [O] INV-21.
  The AWX-job actuator SHALL launch only an AWX job template that appears on the operator-declared
  template-allowlist, and the policy engine (spec/015 `Decide`) SHALL authorize the op-class bound to that
  template; IF a template is not allowlisted OR the policy engine returns a deny verdict for its op-class,
  THEN the actuator SHALL refuse and SHALL NOT launch it.

- **REQ-1705** — [O] INV-02 · [O] INV-06.
  The AWX-job actuator's effect SHALL be expressed as an AWX job-template id plus typed `extra_vars`
  validated against an operator-declared per-template variable schema, SHALL reject any `extra_vars` key
  absent from that schema, and SHALL NOT pass a free-form command string — the argv-equivalent is the
  template plus its typed variables, so a job template is not a shell escape.

- **REQ-1706** — [O] INV-21 · [R] paradigm-rule 8.
  WHEN a classified, policy-authorized AWX-job action reaches the actuation chokepoint, the actuator SHALL
  resolve the AWX API token through the credential engine (spec/016 `Resolve`) AFTER the policy engine
  returns a non-deny verdict and BEFORE it launches the job template, and SHALL launch the template ONLY
  WHILE the mode chokepoint (`safety.MayActuate`) permits actuation — a resolved token is necessary and
  never sufficient.

- **REQ-1707** — [O] INV-09.
  The engine SHALL treat a read-only AWX `setup` / fact-gathering job as a Phase-1-safe SENSOR, and SHALL
  route a mutating job template through the mode chokepoint and the constitutional never-auto floor
  (INV-09) as a mutating channel that is OFF (mode Shadow) until the owner-present flip.

- **REQ-1708** — [O] INV-13.
  The AWX-job lane SHALL authenticate with an AWX token resolved as a `core/config.SecretRef` through the
  sealed store, SHALL NOT hold a plaintext token in configuration, the ledger, or any exportable artifact,
  and the token bound to the read-only sensor / knowledge lane SHALL be a read-only token declared
  distinctly from any launch-capable token.

- **REQ-1709** — [O] INV-10 · [R] paradigm-rule 8.
  WHERE an actuation's effect is not synchronously observable, the lane SHALL return a job handle and the
  engine SHALL poll the job through a deferred-verify channel to a terminal AWX job state (`successful` /
  `failed` / `error` / `canceled`), and SHALL NOT declare the actuation successful at launch time.

- **REQ-1710** — [O] INV-10.
  The engine SHALL treat the job launch as a prediction, SHALL compute the mechanical verdict (spec/002
  `verify.ComputeVerdict`) by comparing the terminal job outcome against that prediction, and SHALL feed the
  resulting verdict to the graduation ladder (spec/015 REQ-1514) as the earned-trust evidence for that
  op-class.

- **REQ-1711** — [O] INV-11 · [O] INV-09.
  WHILE a launched job has not reached a terminal, deferred-verified outcome, the engine SHALL record it in
  a `pending-verification` state and SHALL NOT count it as a clean verified run, and IF the job does not
  reach a terminal state within the operator-declared verification bound, THEN the engine SHALL record it as
  `unverified` and SHALL NOT count it toward graduation.

- **REQ-1712** — [O] INV-07.
  The engine SHALL bind each launch to its `action_id` on the manifest lifecycle chain and SHALL NOT launch
  a second job for an `action_id` that already carries a live or terminal job, so a retry, re-poll, or
  redelivery never double-actuates.

- **REQ-1713** — [O] INV-05 · [O] INV-17.
  The knowledge lane SHALL ingest AWX job templates, their descriptions, and their inventory READ-ONLY into
  the wiki and the RAG retrieval plane so the agent discovers sanctioned runbooks, and SHALL re-read each
  object from the AWX REST API by id at ingest rather than trusting a cached mutable copy.

- **REQ-1714** — [O] INV-21.
  The knowledge lane SHALL NOT launch a job or mutate a target; a runbook it surfaces SHALL enter the
  pipeline ONLY as a proposal subject to the full interceptor chain (REQ-1702), so discovery grants no
  effect.

- **REQ-1715** — [O] INV-19 · [O] INV-13.
  The engine SHALL append every regime resolution, lane selection, job launch, and deferred verdict to the
  tamper-evident governance ledger as a required output, SHALL NOT write a secret value, and SHALL persist
  these to append-only tables on which the runtime database role holds no UPDATE or DELETE.

- **REQ-1716** — [R] paradigm-rule 4 · [O] INV-19.
  The operator console SHALL render the per-target regime-and-lane map, the AWX job-template allowlist with
  each template's authorized op-class, the pending-verification queue of launched-but-unverified jobs, and
  the lane-coverage view answering which lane reaches a target, rendering only real engine state, and every
  allowlist edit SHALL be appended to the tamper-evident ledger.

- **REQ-1717** — [O] INV-02/INV-09/INV-13 · [R] paradigm-rule 8.
  The native-ssh lane SHALL be able to resolve its effect leaf PER ACTION TARGET rather than binding one leaf
  to a single configured host: when enabled by an explicit operator flag (default OFF, behaviour-preserving),
  each governed actuation's leaf SHALL bind to the ACTION's own target host, authenticating with the
  operator-declared ACTUATION identity + key that is DISTINCT from any read/diagnostic identity (the credential
  plane-split — the read path SHALL NOT obtain a mutate credential) and host-key-verified against the
  operator's known_hosts. The per-target leaf's declared actuation host SHALL equal the action target so the
  spec/013 host-match gate (REQ-1219) passes by construction and remains a defense-in-depth floor. The
  per-target leaf SHALL be reached ONLY through the LaneEffect composition seam (REQ-1702) — via an UNEXPORTED
  in-package accessor, adding no exported effect path — so the composition invariant holds. A per-target build
  refusal (no configured actuation identity, an empty target) SHALL be a GOVERNED refusal (Refused, not
  executed, and NOT a Go error that would retry a permanent resolution failure), never a bypass. The lane
  SHALL stay DORMANT until the owner-present mutation flip (REQ-1707): merely resolving a per-target leaf arms
  nothing — the mode chokepoint refuses at Shadow and an empty allowed-units allowlist refuses every unit.

## Persistence contract

The engine writes exactly one immutable `regime_resolution` row per lane selection (the `target`, the
resolved `regime`, the selected `lane`, the matched `rule_id`, and the `resolved` / `refused` outcome), one
immutable `regime_actuation` row per job launch (the `action_id`, the `lane`, the AWX `job_template_id`, the
authorized `op_class`, the launched AWX `job_id`, and the non-secret launch metadata — never the token or a
secret `extra_var` value), and one immutable `deferred_verdict` row per completed deferred verify (the
`action_id`, the `job_id`, the terminal job `status`, the mechanical `verdict`, and the graduation outcome).
Each row is a required output of its function — omitting a field is a Go type error — and is appended to the
tamper-evident governance ledger (INV-19). No secret value is ever persisted; only `SecretRef` references
are stored (INV-13). See [`docs/DATA-MODEL.md`](../../docs/DATA-MODEL.md).

## Regime-composition invariant

A standing check SHALL FAIL if any lane exposes an effect path that reaches an actuator without traversing
the spec/013 interceptor chain (REQ-1702), if the AWX-job actuator launches a template that is not
allowlisted or whose op-class the policy engine denied (REQ-1704), if any `regime_actuation` or
`regime_resolution` row carries a plaintext secret (REQ-1708/1715), if a launched job is counted as a clean
verified run before its deferred verify reaches a terminal outcome (REQ-1710/1711), or if a second job is
launched for an `action_id` that already has one (REQ-1712). The fail-closed property (REQ-1701) SHALL hold
under an unknown or ambiguous regime, and every mutating lane SHALL remain OFF (mode Shadow) until the
owner-present flip (REQ-1707).
