# DATA-MODEL.md — Territory Grounder Postgres State Model

> The state model is the constitution made durable. Every governed-autonomy guarantee
> in this system — the risk band, the fail-closed prediction gate, the mechanical verdict,
> the tamper-evident ledger — is only as strong as the rows that record it and the
> constraints that keep those rows honest. This document defines the one database those
> rows live in.

**Stack:** PostgreSQL + pgvector, one database, one DSN, reached exclusively by the Go
control-plane. This replaces the predecessor's SQLite + FAISS pairing [R] (SOURCE-corrections,
Stack). Temporal owns durable *execution* state (workflow history, timers, signals);
Postgres owns durable *domain* state. The two are complementary — a Temporal workflow is
the deterministic orchestrator's control flow; the rows below are its evidence.

Provenance tags used throughout: **[F]** inherited foundation design · **[R]** product
reframe (multi-user / de-solo) · **[O]** audit-overlay hardening — with source ids
(INV-NN, spec/00x, paradigm-rule N).

Related documents: see **ARCHITECTURE.md** (the Temporal workflow spine and adapter/module
system), **THREAT-MODEL.md** (the trust-boundary invariants this schema enforces), **GOVERNED-BEHAVIORS.md**
(the autonomy bands, prediction gate, and ledger semantics), and
**the-map-is-not-the-territory.md** (the manifesto).

---

## 1. Principles

### 1.1 One database, one DSN, declared per-domain ownership [O] (INV-16)

There is exactly one Postgres database reached through **one DSN**, opened as a single `pgx`
connection pool. Each domain (§4) declares an owning control-plane component; no component
reaches outside its domain except through typed foreign keys. This directly closes the
predecessor's failure mode of three aliased SQLite paths with no proof they were one file
[O] (INV-16, M-03). Shell scripts never touch the database — persistence is a Go-only
capability [O] (INV-02/INV-03).

### 1.2 Schema evolves only through ordered, transactional, deploy-time migrations [O] (INV-16)

Schema changes happen **only** via ordered migrations (golang-migrate / goose) applied in a
startup transaction under an advisory lock (multi-replica safe), never inside request
handling. Runtime `CREATE TABLE IF NOT EXISTS` / `ALTER` in the request path — the predecessor's
error-suppressed pattern — is structurally impossible because **the runtime DB role is granted
DML only and holds no DDL privilege** [O] (INV-16, P2-1). CI fails any migration lacking a
down migration or introducing an unreferenced table.

### 1.3 Referential and domain integrity by construction [O] (INV-16)

Foreign keys are enforced (FK enforcement is on, unlike the predecessor's 5-FK/48-table
SQLite [O] M-03). `NOT NULL`, `CHECK`, and enum constraints encode state-machine legality in
the schema itself, not in application code — e.g. `verdict IN ('match','partial','deviation')`,
the band enum, `schema_version > 0`, and (per §5) `expires_at NOT NULL` on every purgeable row.
An illegal state cannot be persisted.

### 1.4 Authority is checked at the boundary against the acting user/role [R] (paradigm-rule 1)

There is one organization and one estate, so there is **no `tenant_id` and no cross-org
row-level-security isolation**. Each request carries the authenticated principal (user/source +
role) derived from the caller identity, and RBAC authority is checked against that user/role in
the control-plane before any DML — never inferred from a request field. See §2.

### 1.5 Schema versioning is a forward-incompatible-safe discipline [F] (spec/006 REQ-505)

Every session / audit / knowledge / ledger table carries `schema_version INTEGER NOT NULL`,
stamped by its writer, governed by one canonical registry (`CURRENT_SCHEMA_VERSION` +
`SCHEMA_VERSION_SUMMARIES`). Readers **reject** a future-versioned row (`SchemaVersionError`)
rather than silently mis-reading it [F]. Operational rule (carried verbatim from the
predecessor): when the JSON shape of any payload column changes, bump the table's
`CURRENT_SCHEMA_VERSION` **and** add a `SCHEMA_VERSION_SUMMARIES` line describing the change.

### 1.6 DDL is generated from canonical Go types — never hand-maintained in parallel [O] (INV-15)

Each logical entity has **exactly one authoritative definition**: a Go struct with tags.
`sqlc` + a schema-emit tool generate the migrations, the JSON Schema / OpenAPI / AsyncAPI wire
contracts, the in-code validators, and the human-facing counts **from that one struct** [O]
(INV-15). The Postgres model and the API contract are the same source of truth — the console
(`frontend/`) consumes the generated OpenAPI, so there is no second contract to drift [R]
(SOURCE-corrections, UX/API-first). A `testcontainers` round-trip test writes a real row via
the production path and validates it against the generated contract; CI fails on any drift,
hand-written number, or artifact missing a non-null `generated_at` + source hash [O] (INV-15,
M-01/M-05/M-07).

### 1.7 Retention is declared per data class and enforced by an audited purge [O] (INV-14) / [R] (paradigm-rule 5)

`'save everything forever'` is **not a reachable configuration** [O] (INV-14). Every purgeable
row carries `data_class` + `NOT NULL expires_at`; a Temporal cron worker purges expired rows
and alerts if purge lags. The one exception — the immutable audit spine — is retained by
integrity-preserving archival, never deletion. This is the retention split, developed fully in §5.

---

## 2. Multi-user, single-org identity

> **Paradigm rule 1 — MULTI-USER, SINGLE-ORG BY DEFAULT** [R]. One deployment serves one
> organization with many operators, roles, and teams. There is no `tenant_id` and no cross-org
> row-level-security isolation. Authority is checked against the acting user/role; least-privilege
> credentials (per-source, per-agent) are defense-in-depth, not a tenancy boundary.

The predecessor was single-estate *and* single-operator: a bare `issue_id` was a globally unique
correlation key, and one absent human held one shared SSH identity [F→R] (SOURCE-paradigm,
Reframes). TG keeps the single estate but inverts the single operator: many users, roles, and
teams, governed by RBAC.

### 2.1 Tables are org-global; no `tenant_id`

Domain tables in §4 and §5 are org-global — there is **no `tenant_id` column** and no per-org
isolation to enforce. Circuit breakers guarding *shared* infrastructure (the LiteLLM
model-gateway, the platform DB pool) and breakers guarding estate dependencies live in the same
`circuit_breakers` table without any tenancy discriminator.

### 2.2 Authorization is enforced at the boundary, not by RLS

Each request opens its transaction after the control-plane has resolved the authenticated caller
to a principal (user/source + role) derived from the caller identity [O] (INV-01) — never from a
request field. RBAC authority checks resolve against that user/role before any mutating DML runs;
an unauthorized principal is denied at the boundary, not filtered by a `WHERE` clause. `NOT NULL`
FKs on `session_id` keep every event bound to its owning session, so INV-12 still promises "a
given event is acted on at most once" — the guarantee is in the schema constraints and the auth
boundary, not in per-org row filtering.

### 2.3 The correlation key is `external_ref` [R] (paradigm-rule 1)

The predecessor threaded a single `issue_id` — the YouTrack ticket number — as the correlation
key across dedup, session, RAG, blast-radius, and logs [F]. The tracker is now a pluggable adapter
(YouTrack / Jira / GitHub Issues / ServiceNow / native), so the raw id is no longer vendor-fixed.
The universal correlation key is therefore **`external_ref`** — the tracker-adapter-supplied
opaque id, unique within the org's own trackers [R] (SOURCE-paradigm, Reframes: Ticketing).
Session identity, resume, dedup, and every join in this document use this key. Idempotency and
resumability — "work is idempotent and resumable via the external ticket id" [F] — port unchanged.

### 2.4 Least-privilege identity is defense-in-depth [O] (INV-13) / [R] (Tension: single shared SSH identity)

Because a single shared actuation credential was precisely the boundary that must not be
crossable, actuation identity is **per-source + per-agent scoped** (per-source HMAC secrets,
per-agent mTLS / scoped tokens, credential-revoke-as-kill) [O] (INV-13) / [R]. The data-model
consequence: `execution_log`, `credential_usage_log`, and every rollback command are bound to the
scoped credential that captured them, so a revoked or compromised agent credential instantly
kills that agent's reach — layered defense within the one org, not a tenancy wall.

---

## 3. The `ActionManifest` — the immutable binding at the center of the model [O] (INV-07)

The single most important net-new table. The predecessor's crown-jewel weakness was that its
committed prediction was bound to a *pre-session hypothetical plan* that the live session was
then free to mutate — `Prepare Result` checked only that *a* prediction artifact existed, never
that it was *for the action being executed* [O] (INV-07, H-03). TG fixes this by making one
content-hashed object the spine every safety stage shares.

**Definition.** A canonical `Action` (normalized target / op / params / band / plan-hash /
prediction-hash / approval-choice / tool-calls / verification) is canonicalized deterministically
and hashed once:

```
action_id = SHA-256(canonicalJSON(Action))
```

`action_id` is threaded **unchanged** through risk-classification → prediction-commit →
approval-poll options → execution authorization → PreToolUse plan-adherence gate → post-action
verdict. Every stage re-derives and asserts `action_id`; a mismatch is a hard fail-closed abort
[O] (INV-07). Temporal carries `action_id` as workflow state, and deterministic replay
guarantees the assertion holds. **Any** mutation of the Action yields a new id that invalidates
all prior approval/prediction and re-enters the gate via a child workflow — *identity, not
existence, is what the gate protects*.

**Table shape (illustrative).**

| column | notes |
|---|---|
| `action_id BYTEA PRIMARY KEY` | `SHA-256(canonicalJSON(Action))`, computed once [O] INV-07 |
| `external_ref TEXT NOT NULL` | joins to session via `external_ref` |
| `normalized_target JSONB` · `op TEXT` · `params JSONB` | the canonical action, sealed |
| `band TEXT CHECK (band IN ('AUTO','AUTO_NOTICE','POLL_PAUSE'))` | the classified autonomy band [F] |
| `execution_class TEXT` | DETERMINISTIC / FAST_AGENT / STANDARD_AGENT / DEEP_INVESTIGATION / HUMAN_LED |
| `plan_hash BYTEA` · `prediction_hash BYTEA` | binds to the prediction record (§4.4) |
| `approval_choice JSONB` · `tool_calls JSONB` · `verification_hash BYTEA` | approval + executed calls + verdict binding |
| `schema_version INT NOT NULL` | §1.5 |
| `created_at TIMESTAMPTZ NOT NULL DEFAULT now()` | append-only; sealed at creation |

The manifest is **append-only and immutable** — sealed at creation, no UPDATE grant. It is
part of the audit spine (§5): it is the derived, de-identified record of *what was authorized*,
distinct from the purgeable raw transcript of *how the agent reasoned about it*. The PreToolUse
plan-adherence gate refuses any tool call whose derived id is not a constant-time match for an
approved manifest hash [O] (INV-07, INV-21). Because a substituted plan produces a new
`action_id`, "approval-of-X replayed to execute-Y" is uncompilable.

---

## 4. The domains

Each domain below is carried forward from the predecessor's data-model summary [F], reframed
multi-user [R], and hardened by the overlay [O]. **Every table in every domain carries
`schema_version NOT NULL` (§1.5)** — stated once here rather than repeated per row.

### 4.1 Session lifecycle state [F] / [R]

The core state machine, keyed to `external_ref` [R]:

- **`sessions`** — one live-session row per active incident (live vs. append-only split).
- **`session_log`** — append-only close-out record carrying `resolution_type` (e.g.
  `auto_resolved`) [F] (spec/003 REQ-203).
- **`session_turns`** — per-turn token/cost accounting, `UNIQUE(session_id, turn_id)`.
- **`session_state_snapshot`** — immutable per-turn pre-mutation snapshots with
  `pending_tool` / `pending_tool_input` for crash-mid-tool replay [F].
- **`session_queue`** — per-`external_ref` resume queue.

**Reframe:** the fixed 5 named concurrency slots become Temporal workers / task-queues with
org-configurable concurrency and fair-share queueing; durable pause/resume and per-turn
immutable snapshots port unchanged, and the predecessor's *open* auto-resume loop closes as a
Temporal `continue-as-new` [R] (SOURCE-paradigm, Tension: session concurrency). The snapshot
tables remain — Temporal subsumes the *resume mechanics*, but the immutable per-turn pre-mutation
record is still first-class evidence.

### 4.2 Risk-decision audit — `session_risk_audit` [F] / [O] · **audit spine (§5)**

One **immutable** row per classification: `risk_level`, `band` (AUTO / AUTO_NOTICE /
POLL_PAUSE), `auto_approved`, `auto_proceed_on_timeout`, `notify_required` (was `sms_required`;
generalized channel-agnostic) [R], `signals_json`, `operator_override`, and the `plan_hash`
that joins to the prediction gate and the `ActionManifest`. This is the single source of truth
for *why a session auto-proceeded*, checked by a band-aware weekly invariant [F] (spec/001).

**Overlay hardening:** the classifier's zero-value band is the most-restrictive
(`POLL_PAUSE`), so any error/panic/unmatched path fails closed; an `[AUTO-RESOLVE]` is *bound
to the specific committed `plan_hash`* it was classified against [O] (INV-09, and the
foundation's INV-07 binding). Part of the immutable audit spine (§5) — never TTL-purged.

### 4.3 Execution undo-stack — `execution_log` [F]

One row per mutating command: `device`, `command`, `pre_state`, `post_state`, `exit_code`,
`rolled_back`, and the exact **`rollback_command`** — reversibility captured *as data at the
moment of action* so auto-rollback is a lookup, not a re-derivation [F]. Each row is bound to the
scoped credential that captured it, so a revoked agent credential kills replay of its commands [R].
The command itself is a fixed
argv vector, never an assembled shell string [O] (INV-02) — the row records typed args, not
`sh -c` text.

### 4.4 Infragraph causal model + predictions [F] / [O] · predictions are **audit spine (§5)**

The substrate the fail-closed prediction gate reasons over:

- **`graph_entities`** / **`graph_relationships`** — typed entities + directed depends-on edges.
- **`infragraph_dynamics`** — learned-dynamics sidecar: delay/recovery percentiles,
  provenance-weighted confidence, bi-temporal `valid_until`.
- **`infragraph_prediction`** *(built — migration `0002`)* — **append-only** prediction log: `plan_hash`-keyed
  action predictions (`kind='action'`) + shadow cascade predictions (`kind='cascade'`), keyed
  `(plan_hash, kind)`. The prediction IDENTITY (`action_id`, host/rule sets, `control_hosts`, `prediction_hash`)
  is written once at commit; the `TP/FP/FN` and **shuffled-graph negative-control columns** (`control_tp` /
  `control_fp`) are the sole verify-time write. `control_hosts` persists on every row so the eval is
  falsifiable by construction (INV-22) [F]. Table name is singular to match the `schema` registry.
- **`infragraph_cascade_stats`** *(built — migration `0002`)* — append-only cascade over-prediction gating:
  a windowed `control_ratio` (`control_tp / real_tp`; `<= 0.5` is falsifiable) + a `falsifiable` flag.

**Overlay hardening:** the workflow enforces `Predict → Approve → Execute → Verify` ordering —
a poll activity cannot start without a persisted prediction for the *bound* `action_id`, and
the acting agent has **no write path** to the verdict columns [O] (INV-10, INV-07). The
prediction log is part of the immutable audit spine (§5).

**Reframe:** the concrete predecessor counts (356 nodes / 414 edges / 102 declared edges) are
provenance, not product. The org graph is **self-populating** — edges discovered from the org's
CMDB / live-config / monitoring adapters, tagged owner + liveness-contract; the confidence-graded
truth-layering and self-expiring edges port unchanged [R] (paradigm-rule 9).

### 4.5 Knowledge & long-term memory → pgvector [F] / [R] · **purgeable operational body (§5)**

- **`incident_knowledge`** — root-cause/resolution keyed by `alert_rule` + `hostname`,
  `confidence`, `tags`, `project`, embedding, bi-temporal `valid_until`.
- **`lessons_learned`**, **`session_transcripts`** (verbatim chunks + embeddings, the 4th RRF
  signal), **`agent_diary`** (per-agent memory), **`wiki_articles`** (compiled, content-hashed).
- **`control_kv`** — generic org control-issue KV store (the predecessor's `openclaw_memory`,
  de-vendored) [R].

**Storage:** embeddings move from inline `TEXT` to **`pgvector`** columns — one HNSW/IVFFlat
index per embedding table [R] (paradigm-rule 1, SOURCE-paradigm "5-signal RRF"). The 5-signal
RRF weights (semantic 1.0 / keyword 1.0 / wiki 0.9 / transcript 0.4 / chaos 0.35) become **org
retrieval-policy rows** rather than host-level env vars.

**Retention:** this entire domain is the **purgeable operational body** — configurable org
TTL + hard-delete / right-to-erasure. The predecessor's "memory never shrinks / indefinite core
KB" is explicitly dropped as a homelab-scale assumption [R] (paradigm-rule 5, Tension: memory).
See §5.

### 4.6 Evaluation & quality [F] · **purgeable operational body (§5)**

- **`session_judgment`** — LLM-judge 5-dimension scores (investigation / evidence / actionability
  / safety / completeness).
- **`session_quality`** — quality composites.
- **`session_trajectory`** — ReAct step-completion grades.
- **`prompt_scorecard`** — per-prompt-surface scorecards.
- **`ragas_evaluation`** — RAG faithfulness / precision / recall / relevance.

Plus the **prompt-optimization trials** substrate:

- **`prompt_patch_trial`** — one active trial per `(surface, dimension)`; deterministic
  hash-bucketed arm assignment and the Welch-t promotion gate are unchanged [R] (SOURCE-paradigm,
  Prompt-patch trials).
- **`session_trial_assignment`** — deterministic hash-bucketed arm assignment, unique per
  `(issue, trial)`; the Welch-t promotion gate is unchanged [F].

The judge-death detector computes the fraction of recently-ended sessions carrying a real local
judgment using **only tables the judge does not write** [F] (spec/004 REQ-305/306) — a
constraint on *which* tables the query reads, preserved here as a schema-level separation of
judge-written from judge-independent state.

### 4.7 Observability event streams [F] / [O] · **purgeable operational body (§5)**

- **`event_log`** — typed append-only internal events (13+ `event_type`s incl.
  `mcp_approval_response` — the vote ledger — and `tier1_suppression`).
- **`handoff_log`** — sub-agent handoff ledger with `input_history_bytes` / compaction /
  `handoff_depth` + cycle limits (≥5 forces `[POLL]`, ≥10 hard-halt).
- **`tool_call_log`** — per-tool duration / exit / error.
- **`otel_spans`** — locally-buffered spans marked `exported_to_otlp`.
- **`chat_events`** *(built — migration `0004`)* — per INV-12: `UNIQUE(source_id, event_id)` idempotent insert with a
  per-`(source_id, room_id)` durable cursor; **no global cursor** [O] (INV-12). This closes the
  predecessor's cross-room approval-misattribution and silent-event-loss class (H-04).

**Overlay:** tracing is default-on (the honest assessment found OTLP delivering ~0.1% of spans)
[O]. High-cardinality by nature → purgeable operational body with an enforced TTL (§5).

### 4.8 Actuator & scheduling ledgers [F] / [O] · **suppression registry is temporally bounded (INV-20)**

Append-only, rate-capped, audited ledgers for the bounded self-healing actuators and
self-learning registries:

- **`escalation_queue`** *(built — migration `0004`)* — dropped-escalation requeue lane (`attempts` / `status` / `eligible_at`);
  a requeue **re-enters the gated pipeline**, never a side channel [F] (spec/003 REQ-206/207).
- **`disk_grow_log`** — auto-disk-grow audit + rate cap.
- **`discovered_scheduled_reboots`** *(built — migration `0004`)* — self-learning reboot-schedule registry (`host` / `cron` /
  `kind`, `observing`/`live`, in-window observations, `kill_switch`, `valid_until`).
- **`suppression_rules`** — the generalized suppression registry. **[O] (INV-20):** every rule
  is a temporally-bounded row with `NOT NULL valid_from`, `valid_until`, `last_verified_at`; a
  rule applies only if currently valid **AND** live-config-confirmed; an expired / unverified /
  contradicted rule **fails OPEN** (incident investigated). This closes the predecessor's stale
  ASA-reboot-suppression class (H-11) and the "open control *issue* = on-switch" mechanism
  becomes an org policy record via the tracker adapter [R].
- **`security_signal_stats`** — generic security-signal suppression counters (the predecessor's
  `crowdsec_scenario_stats`, de-vendored; CrowdSec is one reference adapter) [R].
- **`credential_usage_log`** — credential-usage / rotation tracking.

**Scheduling substrate note [R] (SOURCE-corrections):** the predecessor's Cronicle scheduler is
dropped; Temporal Schedules supply per-job run history / retries / dead-man natively. These
ledgers record *actuator outcomes and learned rules*, not cron plumbing.

### 4.9 Cost ledger — `llm_usage` [F] / [R]

The single source-of-truth per-request token/cost table: `tier`, `model`, `external_ref`,
input/output/cache tokens, `cost_usd`, `recorded_at` — **real tokens only** (no
estimation/fabrication; the DB↔JSON cross-check audits to zero fabricated rows) [F]. Every
consumer reads this one table.

**Reframe:** the predecessor's "Tier-2 = \$0 under a flat subscription" is retired. Every model
call routes through the bundled **LiteLLM model-gateway**, cost is real per-token per provider,
and `llm_usage` becomes the **org chargeback/billing substrate** with quotas and budgets; the
`$5/session` / `$25/day` thresholds become org policy [R] (SOURCE-corrections; SOURCE-paradigm
rule 6).

### 4.10 Reliability substrate [F] / [R]

- **`circuit_breakers`** — named breaker state (`state` / `failure_count` / `opened_at` /
  `half_open_successes`) with half-open recovery and observable Prometheus state; every external
  dependency degrades gracefully [F]. Breakers guard **shared** infra (the LiteLLM gateway, the
  platform DB pool) and estate dependencies alike, in one org-global table [R].
- **Chaos tables** — `chaos_exercises` / `chaos_experiments` / `chaos_findings` /
  `chaos_retrospectives` with pre/post state, MTTD/MTTR verdicts, error-budget, and
  findings→improvement actions. The SLO-validated-drill mechanism ports; the site-specific
  ASA/VTI/BGP injectors become adapter-provided [R]. Optional/reduced in-product.

### 4.11 Learning-module state [F] (reduce-scope) / [R]

`learning_progress` (per-`(user, topic)` SM-2 mastery/easiness/interval + Bloom level)
and `learning_sessions` (audited lesson/quiz interactions). **Reframe:** the reusable
hallucination-gate (verbatim-substring-of-source) and confidence-clarifier are lifted into the
shared judge/quality library; the *pedagogy* is demoted to an optional multi-user,
RBAC-scoped training **module**, out of the governed-autonomy core [R] (SOURCE-paradigm,
Teacher-agent). Keyed per-`(user, topic)` when enabled.

### 4.12 Schema-version registry (cross-cutting) [F] (spec/006 REQ-505)

Not a domain of its own but the cross-cutting `schema_version` column on every table above,
governed by one canonical registry (§1.5). The registry is itself a generated artifact under
INV-15 — its counts are emitted from the migration set, never hand-written [O].

---

## 5. The retention split — audit spine vs. operational body [R] (paradigm-rule 5) / [O] (INV-14)

> **The tension, stated plainly.** The predecessor conflated two things: "memory never shrinks,
> rows are never deleted" (append-only bi-temporal memory) and a durable audit trail. A
> distributable product **must** offer configurable retention/TTL and right-to-erasure — an
> organization cannot hoard raw transcripts forever. Yet the governance guarantees depend on an
> *immutable* record. These collide. The resolution is to split the two stores the predecessor
> merged. [R] (SOURCE-paradigm, Tensions resolved: memory).

### 5.1 The tamper-evident AUDIT SPINE — append-only, immutable, sealed not deleted

A **short, compact, de-identified** record of *what the system decided and why*. Three members:

1. **`session_risk_audit`** (§4.2) *(built — migration `0003`)* — one immutable classification row per
   decision, with `auto_proceed_on_timeout` pinned false by a DB `CHECK` (the poll-never-proceeds invariant
   made structural) and `schema_version > 0`.
2. **`governance_ledger`** *(built — migration `0003`)* — the append-only **SHA-256 prev-row hash-chained** decision log.
   Every governance decision persists `{decision, reason, action_id, withheld_flag}` as a
   *required* output of the decision function (omitting a field is a Go type error) [O] (INV-19).
   The chain is enforced by **no-UPDATE / no-DELETE grants + an INSERT trigger**; a
   `LedgerVerifier` job re-walks it and rejects tampering. `GovernanceChainBroken` **and**
   `GovernanceChainStale` both page critical [F].
3. **`infragraph_predictions`** (§4.4) — the plan_hash-keyed prediction log.

The **`ActionManifest`** (§3) is the binding root that ties these together: it is content-hashed,
sealed at creation, append-only, and de-identified (normalized target/op/params — not raw
transcript text). The full chain **event → classification → prediction → approval → execution →
verification** is reconstructable from the spine alone [O] (INV-19).

**Retention discipline:** the spine is **never deleted to satisfy TTL**. It is preserved by
**integrity-preserving archival / sealing** — segment + notarize + cold-store — under an
org / compliance TTL + legal-hold policy [R] (paradigm-rule 5, Tension resolution). The
hash chain is continuous across archival: sealing a segment records its terminal hash so the
chain is verifiable end-to-end even after old segments move to cold storage. Because only
**derived, de-identified audit facts** live here, the spine can be retained long-term without
holding raw PII.

The chain is org-global: one continuous hash chain over the whole deployment's decisions [R]
(SOURCE-paradigm, Governance decision log).

### 5.2 The purgeable OPERATIONAL BODY — configurable TTL + right-to-erasure

Everything that carries raw content, PII, or high cardinality:

- verbatim `session_transcripts`, `agent_diary`, `incident_knowledge`, `wiki_articles`,
  pgvector embeddings (§4.5);
- `event_log`, `handoff_log`, `tool_call_log`, `otel_spans`, `chat_events` (§4.7);
- `session_judgment` / `session_quality` / `session_trajectory` / `ragas_evaluation` (§4.6);
- learning-module rows (§4.11).

Every such row carries `data_class` + **`NOT NULL expires_at`**; a Temporal cron purge worker
enforces TTL, size caps, and **hard-delete / right-to-erasure**, and alerts if purge lags [O]
(INV-14). A typed `Redactor` middleware minimizes sensitive fields (prompts, model output,
command traces, hostnames, tokens) **before write** [O] (INV-14, S8-4). A CI policy test fails
any write path that stores an unredacted sensitive field or a TTL-less row.

### 5.3 The boundary, drawn explicitly

| property | audit spine | operational body |
|---|---|---|
| members | `session_risk_audit`, `governance_ledger`, `infragraph_predictions`, `ActionManifest` | transcripts, diaries, knowledge, wiki, embeddings, event/tool/otel/chat streams, eval rows |
| mutability | append-only, immutable, hash-chained | append-only rows, but bulk-purgeable |
| content | derived, de-identified decision facts | raw, PII-bearing, high-cardinality |
| retention | integrity-preserving **archival/sealing**; never deleted to meet TTL | configurable **TTL + hard-delete / right-to-erasure** |
| enforced by | no-UPDATE/no-DELETE grants + INSERT trigger + `LedgerVerifier` [O] INV-19 | `data_class` + `NOT NULL expires_at` + purge worker + `Redactor` [O] INV-14 |
| provenance | [O] INV-19 / [F] hash-chain | [R] paradigm-rule 5 / [O] INV-14 |

The predecessor's bi-temporal `valid_until` mechanism survives inside the operational body:
facts *expire* (a row becomes non-current) independently of *purge* (a row is deleted at
`expires_at`). Expiry is the truth-decay signal used by retrieval and the prediction gate;
purge is the retention/erasure control. They are different columns doing different jobs and must
not be conflated [F]+[R].

---

## Appendix — provenance quick map

- **[F] foundation:** the domains, `session_risk_audit` / `execution_log` / `infragraph_*` /
  `llm_usage` semantics, schema_version discipline, bi-temporal `valid_until`, hash-chain,
  5-signal RRF, spec/001–007 behaviors.
- **[R] reframe:** multi-user + RBAC (no `tenant_id`, no cross-org RLS), `external_ref`
  correlation key, pgvector, org retention & the audit/operational split, de-vendored table names
  (`control_kv`, `security_signal_stats`, `suppression_rules`), Temporal-native concurrency &
  scheduling, org cost/chargeback.
- **[O] overlay:** the `ActionManifest` content-hash binding (INV-07), one-DB/one-DSN + DML-only
  role (INV-16), generate-DDL-from-canonical-types (INV-15), INV-14 mandatory TTL, INV-19
  required-decision-record + tamper-evident ledger, INV-20 temporally-bounded suppression,
  INV-12 per-source cursor + idempotent `chat_events`, INV-09/10/11 fail-closed gate + mechanical
  verdict + typed evidence.
