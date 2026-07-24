# The Constitution of Territory Grounder

**The non-negotiable laws of Territory Grounder (TG).**

> *"The Map Is Not the Territory."* — TG is the one channel that is allowed to tell you no. See [the-map-is-not-the-territory.md](the-map-is-not-the-territory.md) for the manifesto, [ARCHITECTURE.md](ARCHITECTURE.md) for how these laws are realized in Go/Temporal/Postgres, and [CONTRIBUTING.md](CONTRIBUTING.md) for the build-culture half of our values that deliberately does *not* live here.

---

## 1. Preamble, layer model, and precedence

Territory Grounder is an open-source, self-hosted, **single-organization, multi-user** governed-autonomy SRE platform. This document states the laws that no release, RBAC configuration, module, or contributor may violate. Everything else in the repository — architecture, modules, UX, roadmap — is subordinate to it.

### 1.1 The three-layer provenance model

Every substantive claim in TG's documentation is tagged by the layer it originates from, so provenance is auditable and the layering can never be silently re-inverted:

- **`[F]` Foundation** — the design of the predecessor system TG inherits: a deterministic orchestrator driving an untrusted probabilistic model through a gated incident lifecycle. This is battle-tested IP.
- **`[R]` Reframe** — the product paradigm that de-solos and de-homelabs the foundation into a distributable, single-organization multi-user, model-and-vendor-agnostic product. Cited as `paradigm-rule N`.
- **`[O]` Overlay** — the security + quality audit hardening that re-founds every inherited control behind typed, authenticated interfaces bound to one immutable action, making the injection/bypass/drift class structurally uncompilable. Cited as `INV-NN`.

Provenance note (the only place the predecessor's framing may be named): TG's provenance is a solo-operated ~300-machine estate whose single SSH identity, one absent human voting 0/824 approval polls, hand-maintained one-estate graph, and n8n/Cronicle/OpenClaw/Matrix/YouTrack stack **proved** this paradigm. That estate is provenance and battle-test — never the target market `[R] paradigm-mission`. Every solo, single-user, and single-vendor assumption it carried is inverted in the laws below.

### 1.2 Precedence order (highest wins on any conflict)

1. **The Inviolable Mechanical Safety Core** (§3) — non-configurable by any operator, role, contributor, module, or model. `[F]`/`[R] paradigm-rule 8`/`[O]`
2. **The Paradigm Framing Laws** (§5) — the ten laws that make TG a multi-user, single-org product. `[R]`
3. **The Founding Principles** (§4) — the inherited design principles, product-scoped and audit-hardened. `[F]`/`[O]`
4. **Org configuration** — org policy: routing, retention, approver graph, criticality tiers, autonomy flags.

Where a lower layer would relax a higher one, the higher layer governs and the lower configuration is rejected. In particular: no RBAC setting, feature flag, module, or model output can lift a §3 clause. When code and this constitution disagree, **the constitution is the intent and the code is the bug** `[F]`.

---

## 2. Mission

**Territory Grounder is an open-source, self-hosted, single-organization multi-user agentic on-call platform for an organization running infrastructure — from a single homelab operator to a multi-team production estate.** `[R] paradigm-mission / paradigm-rule 1`

TG triages alerts, investigates root causes, and proposes or autonomously executes **reversible** fixes, escalating to the organization's configured on-call human(s) and approver roles only for the genuinely irreversible or security-sensitive. `[F]`/`[R] paradigm-rule 2`

One estate-agnostic, model-agnostic orchestration spine serves any alert domain — the ChatOps / ChatSecOps / ChatDevOps names are example domains, not three codebases `[F]` — through pluggable adapters for ingest sources, ticketing, notification/approval, CMDB, actuation, and model providers, all resolved by **org policy** `[R] paradigm-rule 3`. The spine is a deterministic orchestrator that owns the effect channel driving an untrusted probabilistic model, with a mechanical never-auto floor, fail-closed predict-then-verify, and a two-lane (advisory-open / remediation-closed) safety model (§3), hardened by a separate security+quality audit overlay: per-agent identity/mTLS, RBAC/config controls, org-global state, and a tamper-evident append-only governance ledger separated from purgeable operational memory. `[O] S8-preserve-meta`

Adopters are operators **and** teams who want governed autonomy they can trust and audit — not a black box. Local-first-$0 is one selectable cost/locality mode, never the mission `[R] paradigm-rule 6`. A growing backlog of un-actioned *reversible* work is defined as a **policy failure, not a success** `[F]`.

---

## 3. The Inviolable Mechanical Safety Core

**These clauses are non-configurable by any operator, role, module, contributor, or model. No feature flag, RBAC policy, or model output can lift them.** They are enforced by construction, not by author discipline. `[R] paradigm-rule 8` / `[O] S8-preserve-meta`

### 3.1 The deterministic orchestrator owns the effect channel; the model is untrusted

Effect authority lives only in the deterministic Go/Temporal control-plane. The probabilistic model **only proposes and communicates — it never holds its own effect channel** (isomorphic to an L1→L2→SRE rotation) `[F]`. No LLM-produced token ever becomes control flow, a command string, or a query fragment; model output enters the system exclusively as typed, validated, delimited data, and all action authority is decided by typed policy **outside** the model — there is no "sanitize the prompt" step on the trust path `[O] INV-08, S8-1`. Temporal workflows contain deterministic decision logic only and cannot touch the OS; every side effect is an activity against a capability-scoped adapter `[O] INV-02`.

### 3.2 Reversibility is the primary dial; the mechanical NEVER-auto floor

Reversibility is the primary risk dial, alongside blast-radius, statefulness, and prediction-confidence `[F]`. Irreversible or unrecognized-mutating actions are **never auto, regardless of confidence or any flag** — an unrecognized mutation is NOT "safe" by omission `[F]`. The floor is mechanical and non-configurable; it forces `POLL_PAUSE` and is enforced in defense-in-depth at both the risk classifier and the actuation adapter `[O] INV-09`. The floor set:

> `mkfs` · `dropdb` · `zpool`/`zfs destroy` · `terraform`/`tofu destroy` · `kubectl delete`/`drain` · credential-revoke · reboot/halt · P0/critical-host reboot · `/etc` / config-file overwrite · code/repo destruction · a real jailbreak.

`[F]` / `[R] paradigm-rule 8` / `[O] S8-6`. Automation ceilings are declarative per action-class (never-auto / canary / staged / auto), and **irreversible or stateful classes can never reach the auto ceiling**; an unknown class resolves to never-auto `[O] INV-09`. "P0/critical" is an org-configured criticality tier, but the *mechanism* of the floor is invariant `[R] paradigm-rule 8`.

### 3.3 Two-lane fail model, never blurred

The advisory/triage/context lane **fails OPEN** — on error it degrades to pre-feature behavior. The remediation/mutation lane **fails CLOSED** — it denies action absent a committed prediction. "Don't blur the two lanes" is an explicit invariant `[F]`. Chaos, self-heal, replay, and session-control operations are privileged and fail closed; they are never ordinary ingress paths `[O] INV-01, H-09`.

### 3.4 Predict before acting (fail-closed) and verify mechanically after

The orchestrator commits a `plan_hash`-keyed machine consequence prediction — computed **outside** the LLM — to the append-only prediction log **before any approval poll**; a proposal with no committed prediction is rewritten to a WITHHELD marker and demoted to analysis-only. The remediation lane fails closed. `[F]` / `[O] INV-10`. After execution, deterministic code (never the acting model) writes the **only** `match`/`partial`/`deviation` verdict; the acting agent has **no write path** to its own outcome verdict. A `deviation` is surprise and can **never** auto-resolve. A degree-preserving shuffled-graph negative control is mandatory so the evaluation is falsifiable by construction. `[F]` / `[O] INV-10, S8-3`. Prediction, approval, execution, and verification are all bound to the **same** immutable content-hashed action (§3.5).

### 3.5 One action identity binds every stage

A single canonical `action_id = SHA-256(canonicalJSON(Action))` is computed once and threaded unchanged through risk-classification → prediction commit → approval-poll options → execution authorization → pre-tool enforcement → post-action verdict. Every stage asserts its input's `action_id` equals the id it was authorized for; a mismatch is a hard fail-closed abort. **Any change to the Action yields a new id that invalidates all prior approval/prediction and re-enters the gate — identity, not existence, is what the gate protects.** `[O] INV-07, H-03, S8-preserve-meta`. This is the load-bearing binding lesson: a control is only as strong as its binding to the exact action being authorized.

### 3.6 Gate on structure, not command-string blocklists

The last-line deterministic guardrails fire before permission checks and gate on **structure** — the committed plan (action_id), territory, and egress — never on enumerating bad command strings. The command-string blocklist is retired as the wrong layer of defense; tool composition (chains), not individual calls, is the unit of risk. The pre-execution policy gate is wired **by construction** through a single actuation chokepoint reachable only via the interceptor chain; a control that cannot execute fails **loud and safe** (surfaces an error, refuses the autonomy grant) — never observe-only via a swallowed exception. `[F]` / `[O] INV-21, S8-5`.

### 3.7 Single grammar, parsed once

A proposed action is parsed exactly once, by a single canonical parser, into one typed object every downstream stage consumes; there is **one grammar shared by parser and gate**, and no stage may re-parse raw model text. Approval-poll construction accepts only a gate-constructed object, so "poll without a committed prediction" is uncompilable `[O] INV-06, H-02`.

### 3.8 Autonomy is earned and disabled by default

Autonomous mutation is globally disabled until ingress-auth + action-binding + verification are present and self-test green; a boot-time preflight keeps the system read-only otherwise `[O] INV-09, P0-5`. The mechanical safety core is invariant across the deployment, and every deployment starts on the secure read-only foundation `[R] paradigm-rule 8` / `[O] Phase 0`.

**The active mode IS the sole actuation chokepoint** `[R] paradigm-rule 7` / `[O] INV-09, INV-21`. The former `TG_MUTATION_ENABLED` env knob, the standalone console "Mutation OFF" toggle, and the separate `core/safety.MutationGate` object are RETIRED and ABSORBED into the four-mode autonomy state machine (spec/015 REQ-1520/1521; ADR 0013): exactly ONE state answers "may this action actuate?", so no two states meaning the same thing can drift out of sync. Every obligation the gate held is re-expressed on the mode, fail-closed properties preserved one-for-one — the zero/unknown mode is **Shadow** (read-only, `MayActuate == false` by construction); "may this actuate?" is `mode ∈ {Semi-auto, Full-auto} && preflight-green`; the boot preflight is *proven green* (the chain is wired) WITHOUT enabling actuation, and **enabling** is an authenticated, authority-checked, ledger-audited mode transition, never an env flag; and a deviation-breaker trip or a `/halt` kill-switch **forces the mode to Shadow** (the absorbed `gate.Disable()`). Two defense-in-depth layers stay DISTINCT beneath the chokepoint and are never folded into it: the constitutional **never-auto floor** (§3.2, INV-09) and the **per-action policy verdict** (spec/015) — a policy `auto` cannot execute while the mode is not actuating, and the never-auto floor clamps even in Full-auto.

---

## 4. Founding principles

The inherited design principles `[F]`, **product-scoped** per the reframe `[R]` and each followed by the audit hardening clauses that bind it `[O]`. These sit below the safety core: where a principle would relax §3, §3 wins.

### 4.1 The model is untrusted and gated; deterministic code owns control flow

`[F]` The three-tier architecture — deterministic orchestrator + fast deterministic triage + gates — drives an untrusted model that never holds its own effect channel; historically enforced ×3 (control flow + bypass QA + weekly audit invariant). TG's Temporal workflows are the natural home for the deterministic orchestrator.

**Hardening `[O]`:** No untrusted string ever becomes OS or SQL syntax — actuation is fixed `argv` vectors (never `sh -c`), persistence is bound parameters (never assembled SQL), and no manual quote-escaping helper exists `INV-02, INV-03`. Every external input normalizes to one schema-validated typed envelope whose identifier fields pass explicit grammars before any use; shape-checking is not validation `INV-04`. `StrictHostKeyChecking=no` is not expressible `INV-02`.

### 4.2 One agentic loop, pluggable domains; the mission is domain-independent

`[F]` ChatOps / ChatSecOps / ChatDevOps share one triage → context → propose → approve → execute → verify → learn spine; the trichotomy is a routing/scoping distinction, not three codebases.

**Reframe `[R] paradigm-rule 3`:** the domains are example domains resolved through pluggable adapters, not baked-in verticals.

**Hardening `[O]`:** there is exactly one implementation of each pipeline stage / alert-source type; per-site behavior is supplied as configuration/adapter instances at runtime — no logic is copy-forked per site (NL and GR become two config rows, not two workflows) `INV-18`.

### 4.3 Human as circuit-breaker, not gatekeeper

`[F]` Act autonomously on the safe-and-recoverable, page on the impactful-but-reversible, hold ONLY the irreversible or security event. The three-band gate (`AUTO` / `AUTO_NOTICE` / `POLL_PAUSE`) exists because a backlog of un-actioned reversible work is a policy failure.

**Reframe `[R] paradigm-rule 2`:** the circuit-breaker generalizes from one named human to an **approver graph** — RBAC roles + on-call rotation/escalation + quorum + fallback approver. `AUTO_NOTICE`/`POLL_PAUSE` route to the configured on-call group; veto/approval authority is checked against the acting user/role. Only *who* the circuit-breaker is changes; the mechanics port verbatim.

**Hardening `[O]`:** risk classification is a typed deterministic gate whose only defined error/ambiguity behavior is to escalate to the highest-scrutiny band; the `Band` enum's zero-value is the most-restrictive band, so any error/panic/unmatched path fails closed `INV-09`.

### 4.4 Confidence is a first-class scalar; low confidence terminates or escalates

`[F]` A mandatory, parseable `CONFIDENCE` score with STOP thresholds (below the STOP threshold: stop-and-wait; Tier-1 escalates below its confidence floor or on critical); self-consistency and hedging mismatches are flagged; novelty (`ood:novel-incident`) forces `POLL_PAUSE`. Each tier may decline and escalate but **may never unilaterally stay silent**.

**Hardening `[O]`:** an auto-resolve or high-confidence claim is admissible only if it cites at least one recent, successful, relevant tool-result the **orchestrator itself captured**; a claim backed only by agent free-text or a bare code fence is rejected by construction, and mutating actions require an independent post-condition check `INV-11, M-13`.

### 4.5 Memory accretes; retention is org policy; only the audit ledger is immutable

`[F]` Every turn/session/judgment accretes; history is appended or superseded, never rewritten; bi-temporal validity via `valid_until` (facts expire, rows are not deleted).

**Reframe `[R] paradigm-rule 5`:** split the two stores the predecessor conflated. (a) The **tamper-evident audit spine** — `session_risk_audit`, the SHA-256 hash-chained decision log, and the `plan_hash`-keyed prediction log — stays append-only and is preserved by integrity-preserving **archival/sealing** under an org/compliance TTL + legal-hold policy; the chain is **never broken** to satisfy TTL. (b) The **purgeable operational body** — transcripts, diaries, `incident_knowledge`, wiki, embeddings, and high-cardinality event/tool/OTel streams — honors org-configurable TTL, size caps, and hard-delete/right-to-erasure. Only derived, de-identified audit facts live on the immutable spine; raw PII-bearing memory is purgeable. "Memory never shrinks" and "indefinite core KB" are dropped as homelab-scale assumptions.

**Hardening `[O]`:** every data class has a declared retention policy enforced by an automated, audited purge; every retained record carries `NOT NULL expires_at`; sensitive fields are redacted/minimized before write; "save everything forever" is not a reachable configuration `INV-14`.

### 4.6 Retrieval is multi-signal and rank-fused; trust is graded as weight/rank

`[F]` Multi-signal Reciprocal Rank Fusion (semantic / keyword / wiki / verbatim transcript / chaos baseline) with path-based rank surgery; incident-mined edges capped below the suppression-eligibility cutoff so mined knowledge cannot by itself suppress. Generation survives retrieval/dependency failure via layered fallbacks and **named, observable circuit breakers** — an outage lowers quality but stays deterministic and is not a critical outage.

**Reframe `[R] paradigm-rule 3, 5`:** env weights become org retrieval-policy rows; the seven fixed sources become pluggable source connectors; embeddings move to pgvector; every model (rewrite, rerank, synthesis) resolves through the model-agnostic router.

**Hardening `[O]`:** RAG external calls are guarded by named breakers with persisted state and observable alerts; degradation stays deterministic `INV-21`-adjacent, and no dark path is introduced `INV-22`.

### 4.7 Policy change is externally judged; adaptation is prompt-policy + RAG, never fine-tuning

`[F]` An LLM-judge jury plus one-sided Welch-t A/B prompt-patch trials supply ground truth from **outside** the generator (no self-grading); an explicit ADR forbids weight updates — the order is prompt-engineering → RAG → (fine-tune only if eval proves it necessary). The judge itself is audited and calibrated (TPR/TNR floors), cross-checked by a deliberately non-local higher-capability anchor, and "judge death" is a monitored failure class detected from judge-**independent** tables. The only honest quality signal is a **sealed holdout** the system may never tune to; a >20-point regression-vs-holdout gap is defined as overfitting failure.

**Reframe `[R] paradigm-rule 3, 8`:** the frontier anchor and provider set are org model-routing config; prompt-trial uniqueness/partitioning is keyed by `(surface, dimension)`. Behavioral adaptation is **still** prompt-policy iteration + RAG, never model fine-tuning.

**Hardening `[O]`:** synthetic self-scoring is a smoke test only and can never be the deployment authority; release is gated by adversarial end-to-end coverage of every trust boundary, and every generated artifact carries a non-null `generated_at` + source hash + coverage scope `INV-22, INV-15`.

### 4.8 Suppress noise at the earliest, cheapest point — known-benign only

`[F]` The multi-phase deterministic suppression chain (dedup → operator-declared blast-radius fold → scheduled-reboot phase → known-pattern → active-memory) runs in code before any model spend; **every phase fails open**, only known-benign patterns are suppressed, and critical/unknown always escalate. The residual is verified-and-reopened (two-phase reboot verify reopens and pages on a dirty boot).

**Reframe `[R] paradigm-rule 4, 9`:** the "open control issue = the on-switch" becomes an org suppression-policy record via the ticket adapter; self-learning schedule registries are org-global.

**Hardening `[O]`:** every suppression/maintenance rule is a temporally-bounded row in one authoritative registry with `valid_from`, `valid_until`, and `last_verified_at`; a rule applies only if currently valid **and** live-config-confirmed; an expired, unverified, or contradicted rule fails **open**; suppression knowledge is never hardcoded into a prompt `INV-20`.

### 4.9 Estate knowledge is self-populating; own liveness as a governed set

`[F]` One queryable causal infragraph with confidence-graded multi-source truth-layering, self-expiring automated edges, and mandatory entity resolution is the substrate the prediction gate reasons over. Treat "no data" as a problem, not "no problem": dead-men-watching-dead-men, `absent()` alert clauses, synthetic canaries against an isolated throwaway DB (live-DB-leak counter must stay 0), and a component registry that owns the liveness of the whole federation as a governed set with per-component liveness contracts.

**Reframe `[R] paradigm-rule 9`:** the confidence-graded truth-layering, self-expiring edges, and liveness-as-a-governed-set principles port unchanged; the hand-maintained one-estate inventories are replaced with **self-populating discovery** — graph edges from the org's CMDB/live-config/monitoring adapters, components auto-registered by running Temporal workers/activities/adapters/schedules, and `critical`/`known_dark`/`P0` designations as org config. Specific node/edge counts are provenance, not product.

**Hardening `[O]`:** the `absent()` "no-data ≠ no-alert" guard and canary coverage extend to every TG Temporal workflow/activity, so the port itself introduces no dark component `INV-22`; a startup reconciler compares live registered workflows/adapters against a signed declared manifest and refuses to start on mismatch `INV-17`.

### 4.10 Keep-the-platform-alive is separated in code from act-on-the-mission

`[F]` Plane-A (keep the platform alive) is separated in code from Plane-B (act on the mission); the self-healing actuator is structurally **forbidden** from estate-mutating actions — a grep for mission verbs in the actuator finds none, verified by inspection not trust. Control is bounded and self-escalating and never thrashes: per-target heal-cap → CrashLoopBackOff → escalate; self-learning is bounded by a safety floor + automatic expiry (`valid_until`) + observe-before-live + no-manual-review circuit-breaking; a `≥3×/30d` repeat-offender `(host, rule)` is auto-demoted to analysis-only.

**Reframe `[R] paradigm-rule 7`:** Temporal supplies retries/reconciliation natively; the residual controller heals only non-Temporal things (the model-gateway, a dead module process, a stuck DB pool) via API calls — no host `pkill`/scheduler re-run. The "heal-platform-never-mission" boundary is intact and per control-plane deployment.

**Hardening `[O]`:** the pre/post interception lifecycle is wired by construction so a Plane-A control can never be left unwired or dark; a swallowed exception that silently disables a control is prohibited `INV-21, S8-5`.

### 4.11 Every autonomous decision is auditable by construction; the ledger is tamper-evident

`[F]` One append-only `session_risk_audit` row per classification (signals + `plan_hash` + band), a typed approval-vote ledger, per-stage structured logs keyed by the correlation id, and a SHA-256 hash-chained decision log whose broken **and** stale states both page critical — full traceability from human channel → session → issue → logs → transcripts.

**Reframe `[R] paradigm-rule 1, 5`:** every governance/audit row is org-global; the correlation chain generalizes through the pluggable tracker/notifier adapters; the chain is preserved by integrity-preserving archival, never deletion.

**Hardening `[O]`:** every governance decision produces a persisted structured `Decision` record (decision, reason, `action_id`, withheld-flag) as a **required** output of the decision function — omitting a field is a type error — appended to a tamper-evident ledger enforced by no-UPDATE/no-DELETE grants; a `LedgerVerifier` re-walks the chain and rejects tampering; every router has a total default handler so no input vanishes without a response `INV-19`.

### 4.12 Persist every decision schema-versioned; safety-critical code stays in lockstep with its spec

`[F]` Every session/audit/knowledge table is version-stamped by its writer against a canonical registry; readers reject future-versioned rows (`SchemaVersionError`) rather than silently mis-reading; a content-hash manifest binds governed safety-critical files to their EARS specifications with a semantic (comment-insensitive) drift check.

**Reframe `[R] paradigm-rule 10`:** the spec-code lockstep is re-bound to TG's Go safety-critical files as a **product** safety control, not merely build hygiene.

**Hardening `[O]`:** there is exactly one Postgres database via one DSN with declared per-domain ownership; schema evolves only via ordered transactional migrations at deploy/startup (never inside request handling); the runtime role has DML only and no DDL; referential integrity and state-machine legality are enforced by construction (FK/NOT NULL/CHECK/enum) `INV-16`. Each logical entity has exactly one authoritative definition from which every wire contract, DDL, validator, count, and diagram is generated — never hand-maintained in parallel; CI fails on any drift, uncovered path, hand-written number, or missing provenance `INV-15`.

### 4.13 The unit of work is a durable, correlated ticket; the human is an asynchronous circuit-breaker

`[F]` A durable ticket is both the trigger and the audit sink, and its id is the correlation key across every subsystem; approvals are out-of-band asynchronous reactions on durable pause/resume state, never inline blocking calls. Deterministic code acts on an event before any model does; heterogeneous sources fan into one normalized pipeline (normalize → dedup → flap → burst → correlate → cooldown) through thin per-source adapters before any spend.

**Reframe `[R] paradigm-rule 1`:** the correlation key generalizes from a bare `issue_id` to `external_ref` because ticketing is now a pluggable tracker adapter (YouTrack / Jira / GitHub Issues / ServiceNow / native reference adapters) whose ids are neither uniform nor vendor-fixed; the notifier/approval surface is a generic adapter with Temporal signals as the resume primitive; "site" is one descriptive field in the org estate model. **Each adapter and agent carries a scoped, least-privilege credential — per-source HMAC secrets, per-agent mTLS/scoped tokens — with credential-revoke-as-kill.**

**Hardening `[O]`:** a webhook payload is an untrusted claim, never a fact — inbound events are signature-verified, then the canonical entity is re-read by ID from its system-of-record with the platform's own credential before any dispatch; posted mutable fields are discarded `INV-05`. Each inbound event is keyed by `(source_id, native_event_id)` with a per-source durable cursor and idempotent insert; an approval is bound to a specific pending decision by `decision_id` (carrying `action_id` + room) and routed via `SignalWithStart` to exactly the owning workflow; each session is an isolated Temporal workflow keyed by `session_id` — no global cursor, no process-wide `pkill`, no `is_current` pointer `INV-12`.

### 4.14 Prefer the least-autonomous topology that works; sub-agents are tools, not peers

`[F]` **The topology TG ships today is a single native Go ReAct loop** — one agent that investigates with read-only tools and *proposes*, bounded by a per-agent turn budget (at the poll limit force `[POLL]`, beyond it hard-halt); for one organization (ADR-0010) this is the least-autonomous topology that works. The manager pattern (sub-agents-as-tools the manager owns, not peers it hands off to, with Edit/Write structurally withheld from the read-only sub-agents so control never transfers) is a **deferred design, not a shipped capability**: it stays on the evidence-gated HOLD stated in the Reframe below and is not built while one loop suffices. Whichever topology is live, an agent's retirement follows a governed, auditable lifecycle (active → deprecated → decommissioned → archived) with credential-revoke as the kill primitive, dual-run parity, and a bounded rollback window.

**Reframe `[R] paradigm-rule 3`:** the retired multi-tier legacy topology is dropped, and the open-spec / capability-card / versioned-envelope inter-agent contract is **retained as deferred design, not a shipped contract** — no capability card, handoff envelope, or A2A surface exists in code today, and none is built while one loop suffices. Multi-agent delegation stays on an explicit, **evidence-gated HOLD**: it re-opens only when `delegation_precision` / `parallel_speedup` / `multi_agent_quality_lift` show net value ([EXTERNAL-AUDIT-LESSONS.md](EXTERNAL-AUDIT-LESSONS.md) lesson 9). The predecessor's multi-tier sub-agent sprawl was **live-proven inert** (max handoff depth ever 2, thresholds 5/10 never fired, `handoff_log` = 0 rows, its A2A bus abandoned after two weeks — [SYSTEM-MAP.md](SYSTEM-MAP.md)), which is exactly why TG consolidated it into a single loop rather than re-importing it (the consolidation thesis). Federation is built only when a real second agent needs inherited context.

**Hardening `[O]`:** a capability exists only if its adapter is compiled in and explicitly registered at startup — there is no runtime "mode" string, no host trust path for an unregistered backend, and no null/ambiguous activation state; retiring a capability means **deleting its adapter package**, never leaving it dormant; a half-removed subsystem is a latent re-activation vulnerability `INV-17`.

### 4.15 A session is done only when its knowledge is written back

`[F]` A session is not done when it answers — it is done when its trajectory, transcript, traces, tool-stats, and lessons have been persisted for the next one: the Session End harvest feeds trajectory score + LLM-judge + embedded transcript + OTel + tool-call log + `incident_knowledge`/`lessons_learned` + graph knowledge, and a band-aware reconciler transitions the ticket (`AUTO`/`AUTO_NOTICE` → Done, completed/unknown → To Verify, `POLL_PAUSE` → human).

**Reframe `[R] paradigm-rule 1, 5`:** the operational body honors org retention; only de-identified audit facts reach the immutable spine.

**Hardening `[O]`:** tracing is default-on and the recovery loop is closed — per-turn immutable snapshots (`pending_tool`/`pending_tool_input`) drive Temporal `continue-as-new` resume rather than being captured-but-unused; every session is reconstructable from persistence alone `INV-12`, `INV-19`, observability-completeness overlay.

---

## 5. The ten paradigm framing laws

These are the laws that make TG a distributable multi-user, single-org product `[R]`. They sit above the founding principles and below the mechanical safety core. Stated with verbatim intent from `paradigm-rule 1..10`.

1. **Multi-user, single-org by default `[R] paradigm-rule 1`.** One deployment serves one organization with many operators, roles, and teams — not mutually-distrusting tenants sharing an install. There is no `tenant_id` and no cross-org row-level-security isolation. The correlation key generalizes from a bare `issue_id` to `external_ref` (ids are unique within the org's own trackers). Authority is checked against the acting user/role. Least-privilege identity is defense-in-depth, not tenancy: per-source HMAC secrets and per-agent scoped credentials/mTLS, with credential-revoke-as-kill.

2. **Humans are roles, not a person `[R] paradigm-rule 2`.** The human circuit-breaker is an approver graph — RBAC roles, on-call rotation/escalation policy, quorum, and fallback approver. `AUTO_NOTICE`/`POLL_PAUSE` route to the configured on-call group; veto/approval authority is checked against the acting user/role. "No team to page" is the exact assumption the product inverts.

3. **Model-and-vendor-agnostic via adapters `[R] paradigm-rule 3`.** Ship stable adapter **interfaces** plus a small **reference-adapter** set plus an **SDK** — never a hardcoded stack. Every named vendor becomes one selectable backend behind an interface resolved by org config. Keep the single `component → provider/model` source-of-truth resolver; drop the hardcoded planes.

4. **Controls are API/RBAC/config-driven and audited — never host-local sentinel files `[R] paradigm-rule 4`.** Every autonomy toggle and pre-execution gate lives in an org RBAC-gated feature-flag/policy store and service-side gates in the orchestrator, audited on change. The underlying principle is kept verbatim: every autonomy layer is independently, instantly disableable and ships **dark by default** with observe-before-live promotion.

5. **Retention is org policy, not indefinite — and only the audit ledger is immutable `[R] paradigm-rule 5`.** The tamper-evident governance/decision ledger stays append-only and is preserved by chain-integrity archival/sealing, never deletion; all operational memory is governed by configurable TTL + hard-delete/right-to-erasure. "Memory never shrinks" and "indefinite core KB" are dropped.

6. **Local-first-$0 is a mode, not the mission `[R] paradigm-rule 6`.** Demote local/$0/subscription routing from mission to one selectable cost/locality profile. Cost is real per-token per provider; the goal is policy-driven routing (local-first / cloud-frontier-primary / hybrid); the usage ledger is the org chargeback substrate with quotas and budgets.

7. **Greenfield — no legacy-compat; Temporal is the substrate `[R] paradigm-rule 7`.** Drop the n8n engine, the Cronicle scheduler, the retired legacy Tier-1 triage agent, and the operating-mode abstraction entirely. Drop every "reverts to byte-identical legacy behavior" clause — there is no legacy to revert to; the off-state is simply the non-autonomous baseline. Port the gated choreography, durable pause/resume, per-turn snapshots, concurrency-as-workers/task-queues, and scheduling onto Temporal (workflows, signals, schedules, continue-as-new).

8. **The mechanical safety core is invariant and non-configurable `[R] paradigm-rule 8`.** Regardless of org policy: deterministic orchestrator owns control flow and the effect channel; the model is untrusted and only proposes; reversibility is the primary dial with a mechanical NEVER-auto floor no sentinel or config lifts; two-lane fail model (advisory OPEN, remediation CLOSED); predict-before-acting (fail-closed) with mechanical post-hoc verdicts the acting agent can never write; gate on structure, not command-string blocklists. (This law is realized in full in §3.)

9. **Estate knowledge is self-populating, never a hand-maintained one-estate inventory `[R] paradigm-rule 9`.** The infragraph, blast-radius edges, P0 list, component/liveness registry, and tool inventory are all discovered per-deployment from the org's adapters, tagged with owner + liveness-contract. The confidence-graded truth-layering, self-expiring edges, and liveness-as-a-governed-set principles port unchanged; the specific counts are provenance.

10. **Separate build-culture from product-runtime `[R] paradigm-rule 10`.** How TG is developed and self-graded (honesty-over-marketing, C/D scorecards, remediation sprints, verify-agent-generated-doc-claims-in-audits, spec-code lockstep as a build gate) belongs in `CONTRIBUTING`/values. Its runtime analogs — mechanical verdict, marker-parsed-not-trusted-as-authority, agent-output-verified-against-live-source, and safety-critical-file ↔ spec drift guarding — stay first-class here. Never drop the runtime half when relocating the culture half.

---

## 6. Amendment and governance

- **Precedence is absolute.** No amendment to §4 (principles) or §5 (framing laws) may weaken §3 (the mechanical safety core). An amendment that would do so is void. `[R] paradigm-rule 8`
- **Layer tags are mandatory.** Every clause added to this document carries a `[F]`/`[R]`/`[O]` provenance tag with its source id (`INV-NN`, `spec/00x`, `paradigm-rule N`). An untagged normative clause is a defect. `[R]` layer-tag convention.
- **The constitution is intent; the code is the bug.** When runtime behavior contradicts a clause here, the clause governs and the code is corrected — never the reverse. Safety-critical Go files are bound to this document and their owning EARS specs by the content-hash lockstep manifest; a governed file changing without its spec is reported as drift `[F]`/`[O] INV-15, spec/007`.
- **Adversarial assurance gates changes, not synthetic self-scoring.** No amendment that touches a safety boundary ships without an adversarial end-to-end test that drives the actual code path; synthetic self-scorecards are advisory only and can never be the deployment authority `[O] INV-22`.
- **Terminology is fixed.** `ActionManifest`; the execution/investigation posture; the five execution classes `DETERMINISTIC / FAST_AGENT / STANDARD_AGENT / DEEP_INVESTIGATION / HUMAN_LED`; the three autonomy bands `AUTO / AUTO_NOTICE / POLL_PAUSE`; the inviolable mechanical safety core; the module system; Temporal; the LiteLLM model-gateway; the RBAC roles / approver graph — these terms are identical across all TG documents (see [ARCHITECTURE.md](ARCHITECTURE.md), [THREAT-MODEL.md](THREAT-MODEL.md), [ROADMAP.md](ROADMAP.md)). Renaming any of them is an amendment, not an editorial change.
- **Roadmap dependency.** Governed autonomy (mutation enabled) is reachable only after the secure read-only foundation, the typed action-binding spine, and the fail-closed prediction/verification/ledger spine are green — in that order `[O] Phase 0 → 1 → 2`. The constitution does not permit shipping mutation on an unproven base.

---

*Ratified as the founding law of Territory Grounder. Subordinate documents — [ARCHITECTURE.md](ARCHITECTURE.md), [ARCHITECTURE.md](ARCHITECTURE.md), [THREAT-MODEL.md](THREAT-MODEL.md), [ROADMAP.md](ROADMAP.md), [CONTRIBUTING.md](CONTRIBUTING.md) — implement and must never contradict it.*
