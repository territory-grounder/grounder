<!-- spec/020 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/020 — Governed decision tracer (per-workflow packet-tracer + trace archive)

**Owning behavior family:** — (no narrative `BEH-N`; this is a read-only observability family, like the
metrics family — it sits OFF every chokepoint and authorizes nothing).
**Constitution / invariants:** INV-01, INV-08, INV-13, INV-19, INV-22.
**Phase:** Phase 1 (read-only observability — the tracer is off every actuation chokepoint and changes no
mutation posture; the optional export lane is Phase-1-safe and off by default).
**Status:** Draft.

The decision tracer is TG's **packet-tracer for governed autonomy**: for a single incident it stitches
`ingest → classify → agent ReAct cycles → parse → each interceptor gate → policy Decide → credential
Resolve → regime select → execute → verify` into one ordered, click-through walk, and for each step shows
the rule, the matched rules, the rationale, the tools used, the prompts and skills that composed the step,
the model **confidence** against its `min_confidence` threshold, the risk band, the mechanical verdict, each
gate's verdict-and-reason, the in-force ACL bundle version, and the credential *identity* — **references
and schemes only, never a secret value**. The exact step-through UX already exists as the console Workflows
view (`deploy/console/v2/modules/workflows/`), but it is fixture-driven; the correlation spine
(`external_ref` ⋈ `action_id`) exists, but the decision-grade signal that the walk must render is computed
in memory and **discarded at the database boundary**. This spec closes that gap: it persists the per-step
decision signal, adds the per-session detail read endpoint that assembles it, and live-wires the existing
inspector — without changing one line of the decision path it observes.

> **The tracer is READ-ONLY / OBSERVE-ONLY — this is the defining property, not a footnote.** The tracer
> sits OFF every actuation chokepoint. It authorizes nothing, authenticates nothing, lifts no floor, gates
> no launch, and does not change the mutation posture — exactly like the metrics family (`core/observe`,
> `core/metrics`). Emit points are nil-safe side-writes on paths that already run; the read endpoint is an
> authenticated projection over persisted rows. A decision is unaffected by whether it is traced. This is
> stated below as both an explicit non-goal (§ Non-goals) AND a standing invariant (§ Standing-check
> invariant).

## Non-goals (explicit)

- **The tracer is NOT a control surface.** It never authorizes, authenticates, gates, or mutates; it never
  sits on a chokepoint (REQ-2002). The tamper-resistant `governance_ledger` remains the system of record
  for governance decisions; the tracer reads and correlates, it does not adjudicate.
- **Federation / telemetry-export is OUT OF SCOPE for v1.** spec/020 is **local-only**: nothing is shared
  off the instance. The two-layer trace schema (REQ-2017) is shaped so a *future* federated export is
  possible without a rewrite, but federation itself is a separate far-future thesis in
  [`docs/FEDERATION-VISION.md`](../../docs/FEDERATION-VISION.md) (which graduates to `spec/021-federation`
  once its prerequisites are met). v1 shares nothing.
- **Langfuse / OTel is NOT the store and NOT on the decision path.** Any external LLM-observability export
  is an optional, off-by-default Tier-3 lane carrying the redacted LLM subset only (REQ-2020). There is no
  OTel span backbone today (Prometheus aggregate counters only), so an exporter is net-new work that must
  never become the authority and never touch the decision path.

## Owner decisions (recommended defaults inline)

> **DECISION (owner): external LLM-observability export (Langfuse / OTel).** RECOMMENDED DEFAULT: **DEFER**.
> Build the TG-native tracer (this spec) now; make any OTel span backbone + Langfuse-OSS exporter an
> optional Tier-3 connector, **off by default**, over a span backbone that must be built first — there is
> NO OTel backbone today (the emit plane is Prometheus aggregate counters with no `trace_id`/`span_id`).
> Core spec/020 SHALL stand alone on the TG-native store; the tamper-resistant `governance_ledger` remains
> the system of record; the exporter is additive and never on the decision path (REQ-2020).

> **DECISION (owner): RBAC scope for the trace surface.** RECOMMENDED DEFAULT: **a distinct elevated
> `trace-read` role**. The trace aggregates prompt content, credential *identities*, and full ACL rule
> text into one readable surface — a superset of today's read-only console — so it SHALL be gated behind a
> role distinct from the current `AuthReadOnly` surface rather than reusing it (REQ-2014).

> **DECISION (owner): reach `external_ref` onto the actuation / verify side.** RECOMMENDED DEFAULT: **add
> the column**. Add `external_ref` to the actuation-side tables (`governance_ledger` and the regime
> actuation/verify rows) so the `ingest → verify` walk joins end-to-end with no bridge hop, rather than
> mandating a two-table bridge that breaks for non-proposing sessions (REQ-2019).

> **DECISION (owner): versioned / immutable ruleset record.** RECOMMENDED DEFAULT: **spec/020 owns it**.
> `policy_ruleset` is a singleton latest-wins record today, so a past decision cannot be joined to the
> exact ACL document in force when it decided. Both the tracer and the mode-config questionnaire need this
> history, so spec/020 SHALL own a versioned, immutable ruleset record that a `policy_decision` row
> references by version (REQ-2018).

## Requirements

- **REQ-2000** — [O] INV-19 · [R] paradigm-rule 4.
  The tracer SHALL define one canonical per-step trace record whose fields are `rule` (the winning rule
  id), `matched_rules` (the full list evaluated), `reason` (the rationale), `tools` (name · args · result ·
  duration · status per tool call), `prompts` (`prompt_version`, `seed_hash`), `skills`
  (`name@version#id:origin`), `confidence` (the model's `0..1` scalar) with its `min_confidence` threshold,
  `band` (with `composed_band` and `band_mode`), `verdict` (the mechanical match / partial / deviation),
  `gate` (`{gate_id, verdict, reason}`), `bundle_version` (the in-force ACL document version), and
  `credential` (`{source, scheme, ref}`), and the credential fields SHALL carry references and schemes only
  and SHALL NEVER carry a secret value.

- **REQ-2001** — [O] INV-19 · [O] INV-22.
  The tracer SHALL emit one observe-only trace step at each decision boundary that already runs — classify,
  each interceptor gate, each agent ReAct cycle, policy `Decide`, credential `Resolve`, regime select, and
  verify — WHERE each emit is a nil-safe side-write that neither blocks nor alters the boundary it observes,
  and the emitter SHALL degrade to a no-op rather than fail the decision path when the trace sink is absent.

- **REQ-2002** — [O] INV-22 · [O] INV-09.
  The tracer SHALL sit OFF every actuation chokepoint and SHALL NOT authorize, authenticate, gate, mutate,
  lift any floor, or change the mutation posture; the mechanical never-auto floor (INV-09), the policy
  verdict, the credential resolution, and the mode chokepoint SHALL behave identically WHETHER OR NOT a
  decision is traced, and a standing check SHALL FAIL if any emit or read path reaches an actuator or
  returns a value that gates actuation.

- **REQ-2003** — [O] INV-10 · [O] INV-07.
  The tracer SHALL thread the model's `0..1` confidence scalar from the proposal parser into the execute
  `actuate.Request` and SHALL persist it as a `confidence` column on the decision / triage record alongside
  the `min_confidence` threshold, so a sealed session's confidence reads back non-zero rather than being
  clamped to zero at the database boundary.

- **REQ-2004** — [O] INV-19 · [O] INV-06.
  The policy-decision writer SHALL persist the `bundle_version`, the full `matched_rules` list, and the
  decision `reason` that the policy engine already computes in memory, WHERE `bundle_version` names the
  in-force ACL document version (REQ-2018) and `matched_rules` is the complete evaluated rule set, so a
  persisted decision joins back to the exact ruleset and rules that produced it.

- **REQ-2005** — [O] INV-07 · [O] INV-19.
  The policy `Decide` audit SHALL carry the `action_id`, the `external_ref`, and the `principal` on every
  evaluation row it already writes, so the per-evaluation policy record is joinable into the walk by both
  correlation keys rather than orphaned with empty key columns.

- **REQ-2006** — [O] INV-07 · [O] INV-19.
  The manifest-lifecycle writer SHALL backfill the `approval_choice` and the post-execution `verdict`
  columns on the `action_manifest` row, so the two defined-but-unwritten lifecycle fields carry real state
  rather than remaining NULL.

- **REQ-2007** — [O] INV-19 · [O] INV-09.
  The tracer SHALL persist one ordered `interceptor_gate_verdict` row per interceptor gate traversed
  (`{step_index, gate_id, verdict pass|refuse, reason}`) keyed by BOTH `action_id` and `external_ref`, WHERE
  a passing action leaves one ordered row per gate it passed and a refusing action leaves the refusing
  gate's row and NO phantom pass rows for gates past the refusal, so the inspector renders which gates
  passed, which refused, and why, rather than a single terminal free-text reason.

- **REQ-2008** — [O] INV-08 · [O] INV-13.
  The tracer SHALL persist one `agent_step` record per agent ReAct cycle (`thought`, `tool_call` with args,
  `tool_result` or `observation`, `screen_note`) keyed by `external_ref`, WHERE every thought, observation,
  and tool-result SHALL pass through the Scrub / redaction path BEFORE it is written, the record SHALL carry
  no secret value, and the ingested thoughts SHALL be treated as DATA and SHALL NEVER re-enter the decision
  path as control flow.

- **REQ-2009** — [O] INV-19.
  The tracer SHALL persist the `seed_hash`, the `prompt_version`, and the `model_tier` on the session /
  triage record, so the inspector can show which prompts and which model tier composed each step rather
  than requiring byte-reconstruction from Temporal history.

- **REQ-2010** — [O] INV-01 · [R] paradigm-rule 4.
  The tracer SHALL cover queued, live-running, and executed sessions, WHERE executed and historical sessions
  are served by the per-session detail read endpoint (REQ-2011) and queued and running sessions animate from
  real boundary events over the step channel (REQ-2013), so no session state is invisible to the inspector.

- **REQ-2011** — [O] INV-01 · [O] INV-13.
  The tracer SHALL expose one authenticated read endpoint `GET /v1/sessions/{external_ref}` that assembles
  the ordered per-step trace record (REQ-2000) for one session by joining the correlation spine and the
  per-step tables, WHERE the endpoint requires authentication (INV-01), returns the steps in decision-boundary
  order, and projects a non-secret view only — every credential field is a reference or scheme and no secret
  value is returned (INV-13).

- **REQ-2012** — [R] paradigm-rule 4 · [O] INV-15.
  The operator console SHALL render the per-step walk by live-wiring the existing Workflows view to the
  detail read endpoint (REQ-2011) in place of the `WF_RUNS` fixtures, WHERE the fixture is retained as the
  offline render contract and the change SHALL respect the `deploy/console/v2/assemble.py` build path so
  `make console-verify` byte-reproduces the served `index.html`.

- **REQ-2013** — [O] INV-01 · [R] paradigm-rule 4.
  The tracer SHALL stream per-session step events for queued and live-running sessions over an authenticated
  channel (a per-session SSE channel or an extension of `/v1/events`), so a queued or running session
  animates from real boundary events rather than a client-side simulation clock.

- **REQ-2014** — [O] INV-01.
  The trace surface (the detail endpoint REQ-2011, the step channel REQ-2013, and the console walk REQ-2012)
  SHALL be gated behind a distinct elevated `trace-read` role separate from the current read-only surface,
  BECAUSE the trace exposes prompt content, credential identities, and full ACL rule text — a superset of
  what the `/v1/sessions` summary exposes — and a principal without `trace-read` SHALL be refused the trace
  surface.

- **REQ-2015** — [O] INV-13 · [O] INV-08.
  The tracer SHALL persist and project ONLY `SecretRef` references and schemes for every credential and
  secret-bearing field and SHALL NEVER persist or return a secret value, WHERE agent thoughts and
  tool-results are run through the Scrub / redaction path before they reach any trace table, so no trace
  table, DTO, or export carries a plaintext secret.

- **REQ-2016** — [O] INV-19.
  The per-step evidence tables (`agent_step`, `interceptor_gate_verdict`) SHALL be append-only, WHERE the
  runtime database role holds no `UPDATE` and no `DELETE` grant on them (the migration `REVOKE`s both), so a
  persisted trace step cannot be silently rewritten or removed.

- **REQ-2017** — [R] paradigm-rule 4 · [O] INV-13.
  The trace schema SHALL separate an ESTATE-SPECIFIC layer (hosts, IPs, topology, credential identities, raw
  traces) from a GENERALIZABLE layer (alert-class → resolution → verified-outcome plus graduated artifacts),
  so a FUTURE federated export of the generalizable layer is possible without a schema rewrite, WHERE this
  separation is a schema property only and v1 shares nothing off the instance (federation is out of scope,
  see § Non-goals and [`docs/FEDERATION-VISION.md`](../../docs/FEDERATION-VISION.md)).

- **REQ-2018** — [O] INV-19 · [O] INV-06.
  The tracer SHALL own a versioned, immutable ruleset record such that each `policy_ruleset` state in force
  is retained under a stable `bundle_version`, so a past `policy_decision` (REQ-2004) joins to the exact ACL
  document that was in force when it decided, WHERE the current singleton latest-wins record cannot join a
  past decision to its historical ruleset.

- **REQ-2019** — [O] INV-07 · [O] INV-19.
  The tracer SHALL reach the `external_ref` correlation key onto the actuation / verify side by adding an
  `external_ref` column to the actuation-side rows (`governance_ledger` and the regime actuation / verify
  records), so the `ingest → verify` walk joins end-to-end by one key with no bridge hop and remains
  complete for a session that did not seal an action.

- **REQ-2020** — [O] INV-13 · [O] INV-22.
  IF the optional export lane is enabled, THEN it SHALL be an off-by-default Tier-3 connector over an OTel
  span backbone that carries the REDACTED LLM subset ONLY (prompts, model, tokens, tool-calls, latency, the
  ReAct transcript) and SHALL NEVER carry the governance fields, SHALL NEVER become the system of record, and
  SHALL NEVER sit on the decision path; WHILE the lane is disabled (the default) nothing is exported.

- **REQ-2021** — [R] paradigm-rule 4 · [O] INV-10 · [O] INV-22.
  The tracer SHALL provide a read-only confidence CALIBRATOR that joins the persisted agent confidence
  (`session_triage.confidence`, REQ-2003) to the LLM-free mechanical verified outcome (the verify verdict,
  INV-10 — the agent NEVER adjudicates its own outcome) by the `external_ref` correlation key (REQ-2019), and
  SHALL emit a RELIABILITY CURVE binning stated confidence against the verified-clean rate plus a Brier / ECE
  calibration score, WHERE the calibrator adjudicates nothing and gates nothing (observe-only, INV-22) — so a
  stated confidence becomes empirically meaningful ("0.8 resolves ~80%") and the operator gains the evidence
  to decide whether to raise the policy `min_confidence` off zero. The calibrator SHALL NOT be a prerequisite
  for actuation and its absence SHALL leave the decision path unchanged; until the reliability curve is
  populated the confidence gate is expected to remain defaulted off, with action authorization riding on
  graduation, band, the never-auto floor, and the mode chokepoint (the behavioral / verified gates).

## Persistence contract

The tracer widens the durable decision surface and adds two append-only per-step evidence tables, and writes
nothing on the decision path's critical section beyond what already flows there. Specifically: it persists
the `confidence` scalar and the `min_confidence` threshold on the decision / triage record (REQ-2003); adds
`bundle_version`, `matched_rules`, and `reason` to `policy_decision` plus the `action_id` / `external_ref` /
`principal` keys on the policy `Decide` audit (REQ-2004, REQ-2005); backfills `action_manifest.approval_choice`
and `action_manifest.verdict` (REQ-2006); writes one ordered immutable `interceptor_gate_verdict` row per
gate traversed and one immutable `agent_step` row per ReAct cycle, each keyed by the correlation spine
(REQ-2007, REQ-2008); persists `seed_hash` / `prompt_version` / `model_tier` on the session / triage record
(REQ-2009); retains each in-force ruleset under a stable `bundle_version` in an immutable versioned record
(REQ-2018); and reaches `external_ref` onto the actuation-side rows (REQ-2019). Every credential and
secret-bearing field is a `core/config.SecretRef` reference or a scheme — never a value (REQ-2015, INV-13);
every agent thought, observation, and tool-result is Scrubbed before it is written (REQ-2008). The runtime
database role holds no `UPDATE` and no `DELETE` on `agent_step` or `interceptor_gate_verdict` (REQ-2016,
INV-19). See [`docs/DATA-MODEL.md`](../../docs/DATA-MODEL.md).

## Standing-check invariant

A standing check SHALL FAIL if any tracer emit path or read path reaches an actuator, lifts a floor, or
returns a value that gates actuation (REQ-2002); if any trace table, DTO, or export carries a plaintext
secret rather than a `SecretRef` reference or scheme (REQ-2015, REQ-2020, INV-13); if an agent thought,
observation, or tool-result is written to `agent_step` without passing the Scrub / redaction path (REQ-2008,
INV-08); if the runtime role holds `UPDATE` or `DELETE` on `agent_step` or `interceptor_gate_verdict`
(REQ-2016, INV-19); if a refusing action leaves a phantom pass row for a gate past the refusal (REQ-2007); if
a `policy_decision` row carries a `bundle_version` or `matched_rules` that disagrees with the ruleset in
force (REQ-2004, REQ-2018); or if the detail read endpoint returns a session's steps out of decision-boundary
order or without authentication (REQ-2011, INV-01). The observe-only property (REQ-2002) SHALL hold under
every emit point, and the export lane (REQ-2020) SHALL remain off by default.

## Prior art & differentiation

The competitive analysis — how the governed decision tracer differs from LLM-observability platforms
(Langfuse, LangSmith), APM / distributed-trace tools, and SRE incident-timeline products, and why the
governance grammar (ACL rule / floor / composed band / graduation ladder / mode / credential-resolution /
interceptor gate / mechanical verdict) is not expressible in an LLM-span model — is maintained in
[`docs/PRIOR-ART-tracer-and-federation.md`](../../docs/PRIOR-ART-tracer-and-federation.md) (produced in
parallel). This section is a deliberate stub; no competitor claims are made here.
