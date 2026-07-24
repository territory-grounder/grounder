<!-- spec/020 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/020 — Design: Governed decision tracer (per-workflow packet-tracer + trace archive)

How the requirements in `requirements.md` are realized on the Go / Temporal / PostgreSQL stack. Where this
design and the code disagree, the code is the bug and this document is the intent. The tracer is a READ-ONLY
observability family: it OBSERVES the already-built decision path (classify → agent loop → interceptor →
policy → credential → regime → verify) and NEVER changes it. Every emit is a nil-safe side-write on a path
that already runs; the read endpoint is an authenticated projection over persisted rows. Nothing here sits
on an actuation chokepoint, and nothing here changes the mutation posture (REQ-2002).

## Correlation model

The spine already exists but is incomplete. `external_ref` is the Temporal session key (`temporal/
conventions.go`) carried from ingest through classify and the agent loop; `action_id` is the
content-addressed action key carried from parse through the interceptor, execute, and verify. Today the two
halves meet only through two bridge tables (`session_risk_audit`, `pending_decision`) plus `plan_hash` as a
derived cross-check — so a full `ingest → verify` walk resolves ONLY for a session that sealed an action,
and every walk hops two bridge tables.

The design closes this two ways, both additive:

1. **Both keys on every new per-step row (REQ-2007, REQ-2008).** The new `interceptor_gate_verdict` and
   `agent_step` tables carry BOTH `action_id` and `external_ref`, so a per-step row joins into the walk from
   either half without a bridge.
2. **`external_ref` onto the actuation / verify side (REQ-2019, the recommended owner-decision fix).** Add
   an `external_ref` column to the actuation-side rows (`governance_ledger` and the regime actuation / verify
   records). This makes the `ingest → verify` walk a single-key join with no bridge hop and keeps it complete
   for a session that did not propose an action. The bridge tables remain valid; the column removes the
   dependency on them for the common walk.

```
external_ref  ─ ingest ─ classify(session_triage) ─ agent(agent_step) ─ parse
      │                                                                   │
      └──────────────  action_id  ──────────────────────────────────────┘
                          │
   interceptor_gate_verdict ─ policy_decision ─ credential_resolution ─ regime_* ─ action_manifest ─ action_verdict
                          │
      external_ref column (REQ-2019) reaches back onto governance_ledger + regime actuation/verify
```

## New tables

- **`agent_step`** (`core/db/migrations/0026_agent_step.up.sql`, REQ-2008/2015/2016). One row per agent
  ReAct cycle: `{external_ref, action_id, cycle_index, thought, tool_call jsonb, tool_result_or_observation,
  screen_note, model_tier, created_at}`. Every free-text field is Scrubbed before write (REQ-2015). Keyed by
  `external_ref` (+ `action_id` where an action exists). Append-only: `tg_runtime` holds no `UPDATE` /
  `DELETE` (REQ-2016). Single-org (no tenant column); non-secret only.
- **`interceptor_gate_verdict`** (`core/db/migrations/0025_interceptor_gate_verdict.up.sql`,
  REQ-2007/2016). One ordered row per interceptor gate traversed:
  `{external_ref, action_id, step_index, gate_id, verdict pass|refuse, reason, created_at}`. A passing action
  leaves one row per gate; a refusing action leaves the refusing gate's row and NO rows for gates past it.
  Append-only, both keys, non-secret.
- **`policy_ruleset_version`** (`core/db/migrations/0027_policy_ruleset_version.up.sql`, REQ-2018). The
  immutable, versioned ruleset record: `{bundle_version, ruleset jsonb, mode, composed_band, created_at}`.
  Each in-force `policy_ruleset` state is retained under a stable `bundle_version` that `policy_decision`
  references. Append-only. This is the shared dependency the mode-config questionnaire also consumes to
  prove which configuration took effect.

## New / widened columns (Tier-0 additive-safe)

- **`policy_decision.{bundle_version, matched_rules, reason}`** (REQ-2004) + the T-015-13 keys
  **`policy_decision.{action_id, external_ref, principal}`** threaded through `EvalInput` into the
  `AuditedEngine` writer (REQ-2005). All three decision fields are already computed in memory post-item-#12
  and dropped at the writer; this widens the writer, no new decision logic.
- **`confidence` scalar** on the decision / triage record (REQ-2003), plus threading `Confidence` from the
  proposal parser into `actuate.Request` so the policy clamp receives a real value rather than zero. This is
  the keystone shared with confidence-calibration: one column, two consumers (the tracer renders it, the
  calibrator scores against outcomes).
- **`session_triage.{seed_hash, prompt_version, model_tier}`** (REQ-2009).
- **`action_manifest.{approval_choice, verdict}`** backfilled by the lifecycle writer (REQ-2006).
- **`external_ref`** on the actuation-side rows (REQ-2019).

## The read path

- **`GET /v1/sessions/{external_ref}`** (`core/httpapi`, REQ-2011). An authenticated (INV-01) assembler that
  joins the correlation spine (`session_triage` ⋈ `agent_step` ⋈ `interceptor_gate_verdict` ⋈
  `policy_decision` ⋈ `credential_resolution` ⋈ `regime_*` ⋈ `action_manifest` ⋈ `action_verdict` ⋈
  `infragraph_prediction`) into the ordered per-step trace record (REQ-2000) in the `WF_RUNS` node shape
  (`deploy/console/v2/modules/workflows/fixtures.txt` is the render contract). The assembler projects a
  NON-SECRET view only: every credential field is a reference or scheme, never a value (REQ-2015). Steps are
  returned in decision-boundary order.
- **RBAC (REQ-2014).** The endpoint, the step channel, and the console walk are gated behind a distinct
  elevated `trace-read` role — not the current `AuthReadOnly` surface — because the assembled record exposes
  prompt content, credential identities, and full ACL rule text.
- **Step channel (REQ-2013).** A per-session SSE channel (or an extension of `/v1/events`) streams real
  boundary events so queued and running sessions animate from real state; executed / historical sessions are
  served by the detail endpoint alone.

## Console live-wiring

The Workflows view (`deploy/console/v2/modules/workflows/`, `wfView → wfTimeline → wfNodeEl`) already renders
list → run → click-each-step → expand over the `WF_RUNS` fixtures, and the policy module reserves a labelled
`SOON · packet-tracer` empty state. The change replaces the fixture source with the REQ-2011 endpoint and
keeps the fixture as the offline render contract (REQ-2012). Per the console build discipline, edits go to
`modules/*.txt` (never the built `index.html`), and `deploy/console/v2/assemble.py` must byte-reproduce the
served artifact under `make console-verify`.

## Emit points (observe-only)

Each boundary that already runs gains one nil-safe side-write, mirroring the `observe.Emitter` discipline
(REQ-2001):

| Boundary | Emit | Requirement |
|---|---|---|
| classify | `session_triage` confidence / seed / prompt / tier | REQ-2003, REQ-2009 |
| each interceptor gate | one `interceptor_gate_verdict` row | REQ-2007 |
| each agent ReAct cycle | one Scrubbed `agent_step` row | REQ-2008, REQ-2015 |
| policy `Decide` | `policy_decision` bundle_version / matched_rules / reason + keys | REQ-2004, REQ-2005 |
| credential `Resolve` | credential `{source, scheme, ref}` (refs only) | REQ-2000, REQ-2015 |
| regime select | regime / lane resolution row (existing) reached by `external_ref` | REQ-2019 |
| verify | `action_verdict` + `action_manifest` backfill | REQ-2006 |

None of these blocks or alters its boundary; an absent trace sink degrades the emitter to a no-op (REQ-2001)
and the decision proceeds unchanged (REQ-2002).

## Optional export path (Tier-3, off by default)

There is NO OTel span backbone today — the emit plane is Prometheus aggregate counters (`core/observe`,
`core/metrics`) with no `trace_id` / `span_id`, and `go.opentelemetry.io/*` is an indirect dependency pulled
only by OPA/rego. So the optional export lane (REQ-2020) is net-new: it FIRST builds an OTel span backbone
over the `external_ref` + `action_id` spine, then exports the REDACTED LLM subset only (prompts, model,
tokens, tool-calls, latency, the ReAct transcript) to a self-hosted Langfuse-OSS target over OTLP. It carries
none of the governance fields, is never the system of record, never sits on the decision path, and is off by
default. This lane consumes the GENERALIZABLE layer of the two-layer schema (REQ-2017); the estate-specific
layer never leaves the instance.

## Two-layer schema separation (federation-ready, v1 local-only)

The trace schema separates (REQ-2017):

- **Estate-specific layer** — hosts, IPs, topology, credential identities, raw ReAct traces, gate reasons.
  Never leaves the instance in v1.
- **Generalizable layer** — `alert-class → resolution → verified-outcome` plus graduated artifacts, carrying
  no estate identifier. This is the layer a FUTURE federated export (spec/021, see
  [`docs/FEDERATION-VISION.md`](../../docs/FEDERATION-VISION.md)) would re-validate and share. Shaping the
  schema this way now means that future is reachable without a rewrite; v1 shares nothing.

## How this composes with the platform beneath it

| Question | Engine | Layer |
|---|---|---|
| May TG act? | Policy (spec/015) | authorization — unchanged, unobserved-into |
| With what identity? | Credential (spec/016) | authentication — unchanged |
| Through which channel? | Regime (spec/017) | lane selection — unchanged |
| Did the effect match the prediction? | Verifier (spec/002) | mechanical verdict — unchanged |
| **What happened, and why, step by step?** | **Decision tracer (spec/020)** | **read-only observation + archive + inspector** |

The tracer adds only the last row. It reads the outputs of every engine above it, persists the decision
signal they already compute, and renders the walk — it re-decides nothing and gates nothing.

## Out of scope

The authorization decision is spec/015; the credential resolution is spec/016; the regime lane is spec/017;
the actuation chokepoint + mutation keystone are spec/013 + `core/safety`; the mechanical verdict is
spec/002; the ledger + base RBAC are spec/006. Federation / cross-instance sharing is out of scope for v1 and
lives in [`docs/FEDERATION-VISION.md`](../../docs/FEDERATION-VISION.md) (→ `spec/021-federation`). This spec
owns the per-step trace schema, the additive decision-signal persistence, the two per-step evidence tables,
the versioned ruleset record, the `external_ref` actuation-side reach, the authenticated detail read endpoint
and step channel, the `trace-read` RBAC role, the console live-wiring, and the optional (owner-gated) export
lane.
