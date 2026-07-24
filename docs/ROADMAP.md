<!-- Territory Grounder — founding document. Provenance tags: [F] foundation / [R] reframe / [O] overlay. -->

# Territory Grounder — Roadmap

> *"the one channel that is allowed to tell you no."*

This is the strategic build order for **Territory Grounder (TG)** — an open-source, self-hosted,
single-organization, multi-user, governed-autonomy SRE platform (Go + Temporal + PostgreSQL/pgvector; a native agent
loop over a bundled LiteLLM model-gateway; all integrations as loadable modules; a first-class
`frontend/` console). It states the phases, their definition-of-done, the three cross-cutting tracks
that run through every phase, and the discipline that governs sequencing.

For the concrete component layout see **ARCHITECTURE.md**; for the task-level breakdown of Phases 0
and 1 see **EXECUTION-PLAN.md**; for the safety rules the phases exist to enforce see
**CONSTITUTION.md**; for the invariant catalogue see **CONSTITUTION.md**.

Provenance is tagged inline — **[F]** foundation (the predecessor's inherited design), **[R]** the
multi-user, single-org product reframe, **[O]** the audit-hardening overlay — with source ids (INV-NN,
spec/00x, paradigm-rule N) so the layering is auditable and cannot be silently re-inverted.

> **Status note (2026-07-20).** The phase descriptions below are the *strategic plan*, not the live posture.
> The Phase-2 flip has since been executed: actuation is enabled and governed by the **mode chokepoint** (live
> mode owner-set to Full-auto; absent/zero/corrupt ⇒ Shadow, no actuate). Read the "read-only / mutation OFF"
> language below as a description of each phase's *design scope*; the reconciled current state is
> [`docs/BACKLOG.md`](BACKLOG.md) § Verified state.

---

## The organizing principle: subtraction and consolidation before capability

TG is a greenfield reimplementation, not a port, and its predecessor accreted three whole runtime
substrates (an n8n orchestration engine, a Cronicle scheduler, an OpenClaw Tier-1 agent) plus a
sprawl of host-local sentinel files, shell hooks, and DIY watchdog scripts. **The single most
important sequencing rule is that we remove and consolidate the substrate *before* we add
capability on top of it.** [R paradigm-rule 7]

Concretely, each of these is *deleted*, and its responsibility folded into a smaller, load-bearing
core, before the phase that would otherwise depend on it:

| Predecessor runtime | Consolidated into | When |
|---|---|---|
| n8n (Runner / Bridge / Poller / ~14 receivers) | Temporal workflows + activities + the Go control-plane [R] | Phase 0–1 |
| Cronicle (scheduler) | Temporal Schedules (run-history / retries / dead-man native) [R paradigm-rule 7] | Phase 0 |
| gateway-watchdog trap-EXIT heartbeat | Temporal worker/workflow health, keeping the `absent()` "no-data ≠ no-alert" principle [F][R] | Phase 0 |
| platform-controller Plane-A self-healing | a lean residual that heals only non-Temporal things (LiteLLM gateway, a dead module process, a stuck PG pool); "heal-platform-never-mission" intact [F][R] | Phase 2 |
| OpenClaw / operating-mode abstraction | **deleted entirely** — no adapter, no mode string, no dormant path [R][O INV-17] | Phase 0 |
| host-local `~/gateway.*` sentinels + PreToolUse shell hooks | org feature-flags / policy store + service-side gates in the orchestrator, audited on change [R paradigm-rule 4] | Phase 0–2 |

The discipline is enforced, not aspirational: **a capability exists only if its adapter is compiled
in and explicitly registered; retiring a capability means deleting its package, never leaving it
dormant** [O INV-17]. This is the direct answer to the predecessor's "retired OpenClaw path still
executable" failure class. We do not add the next capability while the last one is still half-removed.

---

## Cross-cutting tracks (run through every phase)

Three tracks are not phases — they thread the whole roadmap and are stated once here so each phase
below can reference them.

### Track A — UX / console

The web **console** (served from `frontend/`, TypeScript; framework deferred) is a named product
pillar and a net-new differentiator the predecessor entirely lacked [R]. It is built **API-first**:
the UI consumes the single generated OpenAPI contract — there is no second, hand-written contract
[R][O INV-15].

- **From Phase 0/1**, the API surface is contract-first: every route is authenticated
  [O INV-01] and generated from one typed source of truth [O INV-15]. The console has a real
  contract to consume from the moment the router exists.
- **First console MVP (Phase 0–1) is read-only**: an audit / timeline / approval-preview view over
  the governance ledger and the ActionManifest — matching the phase where the whole platform is
  read-only investigation (`mutation_enabled=false`). It shows the chain
  event→classification→prediction→approval→execution→verification reconstructed from persistence
  [O INV-19] without granting any mutating control.
- **As mutation turns on (Phase 2)**, the console becomes where the differentiator becomes usable:
  the **approval console** (the human circuit-breaker, routed to the org's approver roles) · the
  **ActionManifest timeline/replay** (predicted → approved → executed → verified as one visual
  chain) · the **tamper-evident ledger view** · **explainability** ("why did the agent do this") ·
  **autonomy-band + kill-switch controls** — these controls move *off* host-local sentinel files
  *onto* the UI/API + RBAC [R paradigm-rule 4] · and **org admin**.

### Track B — the module system

Every integration surface — ingest / tracker / notifier+approval / CMDB / actuation /
model-provider / observability — is a loadable, unloadable **module**, never a hardcoded stack [R].
`adapters/` holds the stable module **interfaces**; `modules/` holds loadable **implementations**
plus a small reference-adapter set plus an SDK [R].

- The proposed default mechanism (ADR-0005) is **out-of-process governed plugins**: each module is a
  separate process/container over a stable protocol (gRPC / HashiCorp go-plugin; **MCP** for
  tool/actuation modules), giving runtime load/unload, third-party modules, process isolation, and
  per-module capability scoping [R].
- Modules are **governed by construction**: signed, capability-scoped, RBAC-enabled; a
  disabled or unregistered module has **no execution path** [R]. This reconciles the module system
  directly with the audit rule that a capability exists only if its adapter is compiled in and
  registered [O INV-17] — the module boundary *is* the mechanism that kills the dead-executable-path
  class.
- The track advances phase-by-phase: interfaces + the argv/stdin actuation contract land in Phase 0
  [O INV-02]; the ingest/tracker/notifier interfaces and one-adapter-per-source-type land in Phase 1
  [O INV-04, INV-05, INV-18]; per-adapter least-privilege identity and the pre-execution interceptor
  land in Phase 2 [O INV-13, INV-21]; the signed-manifest reconciler that refuses a mismatched boot
  is the Phase 3 gate [O INV-17].

### Track C — the LiteLLM model-gateway wiring

TG drops the predecessor's Claude-Code subprocess mechanism entirely; the `agent/` service is a
native Go ReAct/tool-calling loop that calls LLM **APIs directly** [R]. **LiteLLM is bundled in
docker-compose as the model-gateway** — one OpenAI-compatible endpoint, N providers, the
auto-fallback ladder as config, retries, rate-limit handling, and **org budgets/quotas** [R].

- The single component→provider/model source-of-truth resolver is kept from the predecessor; the
  three hardcoded planes are dropped for org cost/locality policy [F][R paradigm-rule 3, 6].
- Default user-configurable, org fallback ladder: `z.ai` → `DeepSeek` → `Mistral` →
  `Anthropic` / `OpenAI` / `Grok` / … with automatic fallback on error/rate-limit/outage [R].
  Local-first-$0 is one selectable mode, not the mission [R paradigm-rule 6].
- Wiring by phase: the gateway stands up read-only in Phase 0 (the agent can investigate but not
  mutate); it is the LLMProvider boundary that guarantees **no model-produced token becomes control
  flow, a command string, or a query fragment** [O INV-08]; the per-request usage ledger
  (`llm_usage`, org-global, real-tokens-only) becomes the org chargeback/quota substrate
  as autonomy comes online [F][R].

### Track D — the eval flywheel (capture before score)

The measurement layer (`docs/TESTING-AND-BENCHMARK.md`) is a track, not a final phase — because the
eval flywheel is a **data-capture problem before it is a scoring problem**. Deferring the *scoring* is
correct; deferring the *capture* creates a cold start at Phase 4 with no corpus to score. So the two
are split across the phases [O; F]:

- **Per-feature acceptance oracles — from Phase 0.** Every `spec/NNN` ships godog `.feature` oracles
  that CI runs (the execution-based definition-of-done, ADR-0009). This is the highest-leverage
  measurement and it is present from day one, not deferred [O INV-22].
- **Trajectory-capture schema — Phase 1.** Co-designed with the typed `IncidentEnvelope` (P1-1) and the
  `ActionManifest` (P1-4/P1-7): every session is persisted in a replayable, labelable, site-tagged
  envelope so a corpus *accumulates* from the first real run. Cheap now, expensive to retrofit [R].
- **Diagnosis-only VISR — Phase 1→2.** The correct-diagnosis leg of VISR (§2.1) is measurable as soon
  as the read-only investigation loop exists; the action + confirmed-postcondition legs are structurally
  N/A until mutation turns on.
- **Full whole-trajectory VISR + Agentic Utility — Phase 2.** Once mutation is earned, all three VISR
  legs (diagnosis ∧ appropriate action ∧ independently-confirmed postcondition) become measurable.
- **Sealed holdout + 20-pt overfitting gate + adversarial boundary-coverage gate — Phase 4.** The
  honesty gate needs a tuning flywheel to overfit *from* and a working, mutating system to harden;
  standing it up earlier would score a system that cannot yet act — the predecessor's synthetic-1.0
  theatre (M-14). See `docs/TESTING-AND-BENCHMARK.md` §2.5.

---

## Phase 0 — Secure, read-only foundation

**Goal.** Stand up the Go/Temporal/Postgres control-plane with **every trust boundary closed by
construction and autonomous mutation globally disabled**, so that no capability can ever be added
later onto an unsafe base. The gate must be trustworthy before anything is allowed to mutate.
[O roadmap P0]

The predecessor's safety story was real in intent but false in binding — unauthenticated ingress,
untrusted strings interpolated into shell and SQL, a prediction gate walkable via a second grammar.
Phase 0 makes that entire injection/bypass class *structurally uncompilable* rather than discouraged
by author discipline. [O verdict]

**Scope.**
- Single Go router with mandatory, non-bypassable auth middleware (mTLS client cert *or* per-source
  HMAC over the raw body + timestamp + nonce replay window); **a route without an auth method fails
  to register at boot** [O INV-01, P0-1]. Privileged control ops live on a separate elevated
  listener; chaos/replay/self-heal are internal Temporal signals, never HTTP paths [O INV-01].
- Actuation is **argv-array / validated-stdin-JSON only** — no shell, no string-built commands; the
  adapter `Exec(ctx, argv []string, stdin []byte)` interface; `StrictHostKeyChecking=no` is not
  expressible [O INV-02, P0-3].
- All persistence via **parameterized pgx/sqlc**; CI lint bans string-built SQL and `sh -c`
  [O INV-03, P1-3].
- **One Postgres, one DSN**, golang-migrate/goose in a startup transaction under advisory lock; the
  runtime role has DML only and no DDL; FK/NOT NULL/CHECK/enum integrity by construction
  [O INV-16, P2-1, P2-2]. The schema is single-org — no `tenant_id`, no row-level-security org
  isolation — with the DML-only runtime role as the privilege boundary [R paradigm-rule 1].
- Secrets are runtime references only; gitleaks CI gate; per-adapter least-privilege identities
  [O INV-13, P0-4].
- **Global `mutation_enabled=false` + boot preflight**: the system is read-only investigation until
  auth + action-binding + verification self-test are green [O INV-09, P0-5].
- **Deleted from the build, not disabled**: session-replay, chaos-start/recover, WAL-healer, the
  legacy ticket-trigger, and OpenClaw do not exist in TG [O P0-2, INV-17].

**Cross-cutting this phase.** Track A: the API contract is generated and authenticated, giving the
console a real spec. Track B: the actuation-adapter argv/stdin interface is defined. Track C:
LiteLLM stands up read-only; the agent loop can investigate through the model-gateway but has no
effect channel.

**Definition of done.**
- No handler, planner, adapter, or DB call is reachable without a verified caller identity; a
  misconfigured/absent auth yields a *dead* endpoint, not an open one [O INV-01].
- A CI grep/build gate proves there is no `sh -c`, no `fmt.Sprintf`-into-command, and no
  string-formatted SQL anywhere in the tree [O INV-02, INV-03].
- One DSN is provably the only database; migrations run only at deploy/startup; the runtime role
  cannot DDL [O INV-16].
- Boot preflight passes and `mutation_enabled` is verified `false`; the system triages and reports
  but cannot act [O INV-09].
- The retired/high-impact endpoints are demonstrably absent from the compiled binary, not merely
  turned off [O INV-17, P0-2].

**Graduation gate (mechanical).** `make all` is green (`vet · lint · spec · test · build`), the boot
preflight `grounder --check` passes with `mutation_enabled=false`, and every acceptance scenario for a
Phase-0 requirement reads `present` (not `pending`) in its `spec/NNN/acceptance/_test_mapping.json`.

See **EXECUTION-PLAN.md** for the Phase-0 task breakdown.

---

## Phase 1 — Typed spine and action binding

**Goal.** Make every input a validated typed envelope and every proposed action a single, immutable,
content-hashed object — eliminating the injection and grammar-mismatch classes at the type level.
[O roadmap P1]

**Scope.**
- **One canonical schema-validated `IncidentEnvelope` per source**; strict per-field grammars
  (hostname / IP / rule / issue-id / enum) rejected on mismatch before any downstream use;
  server-side allowlisted maps for rooms/hosts/scripts/ops; the raw event is unexported past the
  ingest package so no later stage can read the unsanitized body [O INV-04, P1-2]. Correlation keys
  generalize from a bare `issue_id` to **`external_ref`** — a tracker-adapter-supplied id, unique
  within the org's own trackers [R paradigm-rule 1].
- **A webhook payload is an untrusted claim, never a fact**: per-source HMAC with distinct secrets +
  replay window, then re-fetch the canonical entity by ID with the platform's own credential before
  any dispatch; posted mutable fields are discarded [O INV-05, P1-1]. Temporal idempotency keys
  dedupe replays.
- **A single `ParseProposal` → typed `Proposal`** via constrained model tool-calls; **one grammar
  shared by parser and gate**; `BuildApprovalPoll` accepts only a `GatedProposal`, so "poll without
  gate" is uncompilable [O INV-06, P1-5]. This closes the crown-jewel bypass (the second poll
  grammar) at the type level.
- **The immutable content-hashed `ActionManifest`** — `action_id = SHA-256(canonicalJSON(Action))`
  computed once and threaded unchanged through risk-classification → prediction → approval → execution
  authorization → pre-tool enforcement → post-action verdict; every stage asserts its input's
  `action_id`; any change to the Action mints a new id that invalidates prior authorization and
  re-enters the gate [O INV-07, P1-4]. This is the load-bearing binding lesson: identity, not
  existence, is what the gate protects.
- **Per-`(source, room)` durable cursors**, idempotent event insert `UNIQUE(source_id, event_id)`,
  and **session-per-Temporal-workflow** isolation keyed by `session_id`; no global cursor,
  no `pkill` [O INV-12, P1-6]. Fixed 5-slots [F] become Temporal workers/task-queues with
  org-configurable concurrency + fair-share queueing [R].
- **One adapter per alert-source type** driven by a site config row — NL and GR become two
  config rows, not two forked workflows [O INV-18, P2-6].
- Port the sequential gated choreography onto Temporal as gated activities:
  `lock → cooldown → RAG → classify → commit-prediction → build-prompt → run-agent → parse →
  validate → screen → prediction-gate → post` [F][R].

**Cross-cutting this phase.** Track A: the read-only console gains the ActionManifest timeline
(predicted/approved/executed threaded by `action_id`). Track B: ingest / tracker / notifier
interfaces and the one-adapter-per-source-type rule land. Track C: the agent loop's typed
tool-call proposals flow through the model-gateway; usage is metered per request.

**Definition of done.**
- Every external input is a typed struct with per-field grammar validation; a missing required field
  is a compile-time absence or a loud runtime rejection, never a silently-empty interpolation
  [O INV-04].
- Inbound events are signature-verified and canonically re-fetched by ID before any dispatch
  [O INV-05].
- There is exactly one proposal grammar; unparseable/non-manifest-expressible model output is
  rejected, never routed through a looser fallback [O INV-06].
- The `action_id` binds prediction, approval, execution, and verification to the *same* action; a
  substituted plan provably re-enters the gate [O INV-07].
- NL/GR (and any two sites) are config rows over one implementation; a CI parity test
  asserts only declared keys differ [O INV-18].

**Graduation gate (mechanical).** `make all` green; the `IncidentEnvelope` and `ActionManifest` specs
(spec/006, spec/001-002) have their Phase-1 acceptance scenarios `present`; the trajectory-capture
schema (Track D) persists a replayable envelope; and the diagnosis-only VISR harness runs against the
read-only loop.

See **EXECUTION-PLAN.md** for the Phase-1 task breakdown.

---

## Phase 2 — Governed autonomy

**Goal.** Turn on autonomous action **only** through the fail-closed, action-bound
prediction/approval/verification spine and the tamper-evident ledger. Mutation is *earned* by the
surrounding controls, never assumed. [O roadmap P2]

**Scope.**
- **Fail-closed graded `RiskClassifier`** returning a `Band` enum whose zero-value is the
  most-restrictive band (POLL_PAUSE), so any error/panic/unmatched path fails closed; declarative
  per-class automation ceilings (never-auto / canary / staged / auto); **irreversible/stateful
  classes can never reach the auto ceiling; unknown class ⇒ never-auto** [O INV-09, S8-2, S8-6].
  The three bands are **AUTO / AUTO_NOTICE / POLL_PAUSE** [F spec/001]; the human circuit-breaker is
  the org's **approver graph** — RBAC roles + on-call rotation/escalation + quorum + fallback
  approver — not one named person [R paradigm-rule 2]. A growing POLL_PAUSE backlog of *reversible*
  work is defined as a policy failure, not a success [F].
- **The inviolable mechanical safety core is invariant and non-configurable** [R paradigm-rule 8]:
  reversibility is the primary dial with a mechanical **never-auto floor** (mkfs, dropdb,
  zpool/zfs destroy, tofu destroy, kubectl delete/drain, credential-revoke, config-overwrite,
  reboot/halt, P0-reboot, jailbreak) that no flag, policy, or org setting lifts, enforced at both
  the classifier and the actuation adapter (defense in depth) [F][O INV-09]; the two-lane fail model
  is never blurred — advisory fails OPEN, remediation/mutation fails CLOSED [F][R].
- **Distinct pre-execution prediction and post-execution verification activities**; the Temporal
  workflow enforces `Predict → Approve → Execute → Verify` ordering; a deterministic pure function
  computes the **match / partial / deviation** verdict; the LLM never adjudicates its own outcome;
  **deviation ⇒ never auto-resolve** [O INV-10, S8-3]. The shuffled-graph negative control stays
  mandatory so the eval is falsifiable by construction [F].
- **Wired-by-construction pre/post interception** (admission → territory/egress/policy → execute →
  audit); the actuation adapter's single `Execute(ctx, ActionManifest)` chokepoint is reachable only
  through the Go interceptor chain; a startup self-test fails boot if the gate is unwired, so a dark
  control is impossible [O INV-21, S8-5]. The command-string blocklist stays retired — gate on
  **structure** (committed plan, territory, egress), not on enumerating bad command strings [F][R].
- **Typed `Evidence` records**: an auto/high-confidence claim is admissible only if it cites at least
  one recent, successful, relevant orchestrator-captured `ToolResult` ID; a bare code fence or agent
  free-text is rejected by construction; mutating actions get an independent post-condition check
  [O INV-11, P3-3].
- **Hash-chained append-only `governance_ledger`** with a `LedgerVerifier`; every governance decision
  is a *required* output of the decision function (decision + reason + `action_id` + withheld-flag),
  appended under no-UPDATE/no-DELETE grants; every router/dispatch has a total default arm so no
  input vanishes silently [O INV-19, M-11]. The immutable audit spine is preserved by
  integrity-preserving archival/sealing, never deletion [R paradigm-rule 5].
- **Temporally-bounded, live-verified suppression registry that fails open** — a rule applies only if
  currently valid AND live-config-confirmed; an expired/unverified/contradicted rule fails open to
  investigation; suppression knowledge is never hardcoded into a prompt [O INV-20, H-11].
- **Flip `mutation_enabled` to true only after the Phase-0 preflight passes green** [O INV-09, P0-5].
- Autonomy toggles are **API/RBAC/config-driven, org-scoped, audited on change** — never host-local
  sentinel files; the ships-dark-by-default + observe-before-live promotion principle is kept, the
  byte-identical-legacy-revert clause is dropped (greenfield: the off-state is simply the
  non-autonomous baseline) [R paradigm-rule 4, 7].

**Cross-cutting this phase.** Track A: the console becomes operational — approval console routed to
approver roles, ActionManifest replay, ledger view, explainability, autonomy-band + kill-switch
controls, all API/RBAC-gated. Track B: per-adapter least-privilege identity/mTLS +
credential-revoke-as-kill; the pre-execution interceptor every actuation adapter must traverse.
Track C: org budgets/quotas enforced against the usage ledger before mutating spend.

**Definition of done.**
- The classifier fails closed on every ambiguous/error path; irreversible/stateful actions provably
  cannot reach auto; the never-auto floor is enforced at classifier *and* adapter [O INV-09].
- Prediction persists before approval; verification runs after execution; the verdict is written
  only by the verifier; a deviation is never auto-resolved [O INV-10].
- The pre/post interceptor is wired-by-construction; boot fails if the gate is absent; no control can
  be left dark [O INV-21].
- Auto-resolve claims cite orchestrator-captured `ToolResult` IDs re-checked for
  provenance/recency/success/relevance [O INV-11].
- The full chain event→classification→prediction→approval→execution→verification is reconstructable
  from the ledger alone, and the `LedgerVerifier` rejects tampering [O INV-19].
- `mutation_enabled` is on *only* behind the proven, action-bound gate.

**Graduation gate (mechanical).** `make all` green; spec/001-005 acceptance scenarios `present` and
passing; the full 3-condition whole-trajectory VISR (§2.1) runs on the replay corpus per stratum;
`mutation_enabled=true` only after the Phase-0 preflight is green (asserted at boot).

---

## Phase 3 — Anti-drift, single-source-of-truth, decommission discipline

**Goal.** Guarantee that docs, contracts, counts, and the running system can never disagree — and
that nothing ships retired-but-present. [O roadmap P3]

**Scope.**
- **Generate** DDL, JSON Schema / OpenAPI / AsyncAPI, in-code validators, README count blocks, and
  architecture diagrams from **one typed source per entity**; CI fails on drift, uncovered paths, or
  any hand-written number [O INV-15, M-05, M-07, P2-5]. This is the contract the `frontend/` console
  consumes (Track A) — one contract, generated, never a second hand-written one [R].
- **100% endpoint contract coverage** with declared auth/error/idempotency schemas; every generated
  artifact embeds a non-null `generated_at` + source hash + coverage scope [O INV-15,
  D9-contract-authority].
- **Manifest reconciler** (Track B gate): the live registered adapters/workflows must match a signed
  declared manifest or the release fails; a CI grep+build gate forbids retired identifiers; a
  `find_dead_code` gate rejects unreachable code [O INV-17, M-12, P2-4]. This is where "retiring a
  capability = deleting its package" becomes a build-blocking check.
- **Retention/classification policy per data class** with an automated, audited purge worker; every
  retained record carries `data_class` + a NOT NULL `expires_at`; CI asserts no untagged or TTL-less
  sensitive write; "save everything forever" is not a reachable configuration [O INV-14, P3-4]. This
  operationalizes the split the predecessor conflated: the tamper-evident **audit spine**
  (`session_risk_audit`, hash-chain, prediction log) is preserved by archival/sealing under an
  org/compliance TTL + legal-hold policy, while the **purgeable operational body**
  (transcripts, diaries, incident_knowledge, wiki, embeddings, event/tool/otel streams) honors
  org TTL + hard-delete/right-to-erasure [R paradigm-rule 5].

**Definition of done.**
- Every wire contract, DDL, validator, count, and diagram round-trips losslessly from one typed
  source; CI fails on any hand-maintained parallel copy, uncovered path, drift, or missing provenance
  [O INV-15].
- The running system's registered adapters/workflows exactly match the signed manifest, or it refuses
  to start; no retired identifier survives in a compiled package [O INV-17].
- Every persisted row has a declared data class and an enforced TTL; the purge worker runs and alerts
  if it lags [O INV-14].

**Graduation gate (mechanical).** `make all` green; the generate-and-diff drift gate, the manifest
reconciler, and the per-class TTL gate all pass with zero hand-written counts and zero retired
identifiers in any compiled package.

---

## Phase 4 — Adversarial assurance gate

**Goal.** Replace test theatre with executable, adversarial, boundary-complete coverage, and demote
synthetic self-scoring to advisory. The predecessor's suite certified green by asserting on source
strings and synthetic streams while the exported workflow contained the actual bypass; TG's release
authority is adversarial end-to-end coverage of every trust boundary. [O roadmap P4, verdict, M-02,
M-14]

**Scope.**
- **Executable e2e against the running stack** (Temporal test env + ephemeral Postgres) with
  malicious, concurrent, replayed, delayed, and partial-failure fixtures; **no source-string
  assertions; governed code cannot be excluded** from the runnable suite [O INV-22, M-02, P3-1].
- **A shared negative-fixture / fuzz corpus** (metacharacters, separators, newlines, Unicode,
  oversized, duplicate/replay) fired at every ingress and actuation boundary; a boundary-coverage
  metric fails CI if any declared trust boundary lacks an adversarial test [O INV-22, P3-2].
- **Round-trip conformance test**: write a real row via the production path → read it back → validate
  against the generated contract [O INV-15, M-01].
- **The synthetic canary** runs against an isolated ephemeral Postgres (never the live DB, live-DB-leak
  counter must stay 0) as a **low-weight advisory** Prometheus metric only; the CI/CD deployment gate
  requires the adversarial e2e/negative suites + a production-like canary to pass [O INV-22, M-14,
  P3-5]. Synthetic self-scoring can never be the deployment authority.
- **Ledger tamper-replay test** asserts chain rejection; a **prediction-gate bypass property test**
  enumerates every parser path and asserts gate-then-poll ordering [O INV-22, INV-06, INV-19].
- **Safety-critical file ↔ spec lockstep** re-bound to TG's Go files: a content-hash manifest binds
  every safety-critical Go file to its owning EARS spec so the audit's own hardening cannot silently
  diverge from spec [F spec/007][R paradigm-rule 10].

**Definition of done.**
- Every safety control (ingest auth, injection boundaries, routing, prediction gate, verdict,
  banding, ledger chaining) is exercised by a test that drives the *actual* code path with hostile,
  concurrent, replayed, delayed, and partial inputs and asserts on observed output/state [O INV-22].
- Every declared trust boundary has at least one adversarial test; the boundary-coverage metric is at
  its floor; no governed file is excluded from the suite [O INV-22].
- The deployment gate is the adversarial suite + a production-like canary — not the synthetic
  scorecard [O INV-22].

**Graduation gate (mechanical).** The release bars in `docs/TESTING-AND-BENCHMARK.md` §5 all hold:
boundary-coverage map ≥ 1 adversarial test per declared trust boundary (all green), no `>20pt`
regression-vs-holdout gap on the latest sealed-holdout run, judge calibration floors hold, and the
synthetic canary is green but not counted as authority.

---

## Sequencing summary

```
        subtraction/consolidation ───────────────────────────────────────────►
        (delete n8n · Cronicle · OpenClaw · sentinels BEFORE adding capability)

  P0  Secure read-only foundation      mutation_enabled=false · auth-by-construction
   │      · no-shell/no-string-SQL · one DSN (single-org) · secrets-as-refs
   ▼
  P1  Typed spine + action binding      IncidentEnvelope · ParseProposal · ActionManifest(action_id)
   │      · session-per-workflow · one-adapter-per-source
   ▼
  P2  Governed autonomy                 fail-closed banding · predict/verify · ledger · flip mutation ON
   │      · mechanical never-auto floor (invariant) · wired-by-construction interceptor
   ▼
  P3  Anti-drift / single-source        generate-everything · manifest reconciler · per-class TTL purge
   │
   ▼
  P4  Adversarial assurance             executable boundary-complete e2e · canary advisory-only · spec lockstep

  Track A  UX/console ......... API-first from P0 · read-only MVP → operational approval console at P2
  Track B  module system ...... interfaces P0 → adapters P1 → identity/interceptor P2 → signed-manifest gate P3
  Track C  LiteLLM gateway ..... read-only P0 → typed tool-calls P1 → org budgets/quotas P2
  Track D  eval flywheel ...... acceptance oracles P0 → capture schema P1 → diagnosis-VISR P1/2 → full VISR P2 → holdout+coverage gate P4
```

Each phase's definition-of-done is the entry gate to the next. **Phase 2 (mutation) cannot begin
until Phase 0's boot preflight and Phase 1's action-binding are green** — this is the whole point of
"the gate must be trustworthy before anything mutates" [O INV-09, P0-5]. The three cross-cutting
tracks advance continuously so that the console, the module system, and the model-gateway are never
retrofitted onto a finished core.
