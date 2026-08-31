<!-- spec/012 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/012 — Read-only Runner Temporal workflow

**Owning behavior family:** the session Runner (`temporal/runner/`, `cmd/worker/`).
**Constitution / invariants:** INV-07, INV-09, INV-21.
**Phase:** Phase 1 (typed spine). **Status:** Approved.

The Runner is Territory Grounder's deterministic session orchestrator, re-expressed as a Temporal
workflow (replacing the predecessor's n8n Runner). It drives an ingested `IncidentEnvelope` through the
gated read-only pipeline — investigate (the native agent loop) → classify (the three-band classifier) →
gate (commit the machine prediction, seal the content-hashed `ActionManifest`) — and **stops at
propose**: while mutation is off, the execute and verify activities are present but no-op, so an
incident flows end to end to a sealed, classified, gated proposal with no estate mutation. The workflow
body is control flow only; every side effect is a Temporal activity, and no activity executes an OS
command. This document is the requirement source of record; the design is in `design.md`, the runnable
acceptance oracles are in `acceptance/`, and the engineering tasks are in `tasks.json`.

## Requirements

- **REQ-1101** — [O] INV-09/INV-21 · [F] "session orchestrator (the Runner)".
  The Runner workflow SHALL drive an ingested incident through investigate → classify → gate to a
  sealed, classified `ActionManifest`, and the execute activity SHALL route through the wired-by-
  construction actuation interceptor chain (spec/013) — NOT a direct OS call. WHILE mutation is off the
  interceptor SHALL refuse at its mutation gate and record the refusal, so the workflow SHALL stop at
  propose and SHALL NOT mutate the estate — but through the real governed chain, not a dark stub.

- **REQ-1102** — [O] INV-21.
  The Runner workflow body SHALL contain control flow only; every side effect SHALL be a Temporal
  activity against a capability-scoped primitive, and no activity in the read-only pipeline SHALL
  execute an OS command.

- **REQ-1103** — [O] INV-07.
  The `action_id` derived for the proposed action SHALL be threaded unchanged into the gate activity,
  and the gate's sealed `ActionManifest` `action_id` SHALL be asserted equal to it; IF they differ, THEN
  the Runner SHALL fail closed and re-enter the gate rather than proceed.

- **REQ-1104** — [F] Runner incident lifecycle · [O] INV-06/INV-07.
  WHEN the agent produces no usable proposal (it stops or escalates without one), the Runner SHALL end
  the session without an action and without mutation. A terminal outcome counts as a usable proposal ONLY
  when it carries a VALIDATED, NON-EMPTY action — the same required fields (external_ref, target, op_class,
  op all non-empty) `ParseProposal` enforces; a stop or a handoff/cycle-limit escalate that carries the
  empty (zero-value) action is NOT a proposal. For such a no-proposal terminal the Runner SHALL seal NO
  `ActionManifest`, commit NO prediction, and open NO approval poll — it SHALL NOT derive an `action_id`
  from an empty action, so a human is never polled to approve nothing. An escalation handoff (the agent hit
  the cycle/poll limit without reaching a proposal) SHALL be recorded with a terminal outcome DISTINCT from
  an ordinary grounded stop, so the handoff is surfaced to the durable triage record, never silently
  swallowed. This does not weaken the proposal path: a genuine non-empty proposal — including a
  low-confidence escalate that carries a real action — SHALL still seal, predict, and (per its band) poll.

- **REQ-1105** — [O] INV-12/INV-19 · [F] the predecessor approval poll (the vote-consuming half).
  WHEN a proposal is classified POLL_PAUSE, the Runner SHALL wait for the authenticated human vote the
  poll solicited, delivered as a workflow signal keyed by the session's external_ref AND naming the
  session's sealed action_id — a vote decides ONLY when it names the gated action, so a blind, premature,
  or stale vote can never release an action the human did not see. A vote naming any other action SHALL
  be ledger-recorded and ignored (the wait continues); a vote arriving after the decision SHALL be
  ledger-recorded as superseded — no vote is ever silently swallowed. An APPROVE SHALL be threaded into
  the execute path as the interceptor admission gate's Approved (the only way a poll-band action may
  proceed); a DENY SHALL stand the session down without mutation; an unanswered poll SHALL time out to
  DENY — never a silent approval. WHILE the poll waits, the Runner SHALL periodically re-check whether the
  incident's SUBJECT already RECOVERED on its own (a provider recovery TG durably captured after the poll
  opened — its OWN evidence, never the model's word, INV-11); on such a self-recovery it SHALL close the
  poll as OBSOLETE — a fail-closed stand-down that authorizes NO actuation, recorded on the ledger — so a
  self-resolved incident's poll is not left parked for the full wait as a stale open decision. The re-check
  SHALL be version-guarded (pre-existing histories replay deterministically) and inert when no recovery
  evidence seam is wired. A vote naming a different action SHALL be COUNTED and ignored (never a
  per-vote ledger write nor scheduled activity — so a flood of misbound votes can neither grow the
  workflow history until the session is terminated nor be timed to a ledger blip to tear down a waiting
  session); the ignored count SHALL be summarized in ONE best-effort ledger record. A sustained flood of
  misbound votes past a bound SHALL stand the poll down as an abandonment (DENY, fail-closed) — an
  attacker can at worst force a deny, never an approval. The operator's terminal decision SHALL be
  appended to the hash-chained governance ledger with at most ONE record attempt (a record failure fails
  the session closed — never a duplicate authorization row), and the execute activity SHALL be attempted
  at most once per session with an idempotent short-circuit on an existing verdict — one approval can
  never execute an action twice.

- **REQ-1106** — [O] INV-19 · [F] the eval harness's LLM-judge (task #26 — the durable judge spine).
  WHEN a Runner workflow reaches a terminal outcome (a no-proposal stop, a stood-down poll decision, or
  a completed proposal), the Runner SHALL persist a compact triage record — external_ref, host,
  alert_rule, band, outcome, proposed, op, cited evidence ids, the grounded no-action conclusion, the
  committed machine prediction rendered judge-readable (present when a proposal reached the gate; empty
  otherwise — TG-61, so the asynchronous judge scores falsifiable_prediction over real data not a floor),
  and the composed skill_load provenance — via a dedicated activity. The record write SHALL be best-effort:
  a persistence failure SHALL NOT fail the session (the record feeds evaluation, never authorization).
  Judge scoring SHALL be asynchronous and read-only over that record — no judge call SHALL run inside
  the Runner workflow, and a judge outage SHALL leave sessions unjudged (honest, never fabricated)
  rather than degrade triage.

- **REQ-1107** — [O] INV-09/INV-21 · [R] readiness §3 (the staged-canary effect-leaf seam, #23).
  The worker SHALL construct the interceptor's effect-leaf actuator as the read-only reference adapter by
  default, and WHERE an SSH host and identity are operator-declared it SHALL instead construct the gated
  SSH mutating actuator with a local argv-only runner and the operator-declared unit allowlist; WHILE
  mutation is off the constructed actuator SHALL report read-only and SHALL execute nothing — behaviorally
  identical to the read-only default.
  The worker SHALL expose exactly one operational path to autonomous mutation, and it SHALL be both
  config-gated and proof-gated: only WHEN the operator has explicitly armed `TG_MUTATION_ENABLED=true`
  AND the interceptor's boot self-test has already proved the interception chain wired SHALL the worker
  enable mutation, and it SHALL do so solely through the proof-gated safety-core entry point, which SHALL
  refuse to flip the switch unless the boot preflight is green (the switch and the preflight-green bit move
  together, atomically, inside the safety core). Any refusal by the safety core, and any `TG_MUTATION_ENABLED`
  value other than exactly `true` or `false`, SHALL fail the worker boot CLOSED. The default — the flag
  unset or `false` — SHALL keep the worker read-only; enabling mutation SHALL NOT happen at boot absent the
  explicit arming, so the system ships OFF (observe-before-live) and is turned on only by an operator.
  **Absorbed into the mode chokepoint (spec/015 REQ-1520/1521; ADR 0013; TG-112).** The
  `TG_MUTATION_ENABLED` knob and the mutation-binary vocabulary above are RETIRED: "may this actuate?" is
  derived from the owner-set 4-mode state (mode ∈ {Semi-auto, Full-auto} AND preflight-green AND
  breaker-not-tripped), set only through the RBAC- and ledger-gated ModeController — never an env flag.
  The paragraph above is retained as the historical shape of this boot gate; its fail-closed properties
  (ships read-only, malformed arming fails the boot CLOSED) carry forward unchanged onto the mode seed
  path (`TG_INITIAL_MODE`).
  The worker SHALL PUBLISH its live actuation posture — the owner-set mode, the chokepoint's derived
  may-actuate state and the effect leaf's capability (TG-112) — to the durable single-writer
  runtime-posture projection immediately at boot and SHALL re-publish it on a bounded heartbeat so the freshness stamp stays current, so the
  read-only grounder reports the WORKER's true posture (across the process boundary) rather than its own
  gate, which is read-only by construction. A publish error SHALL be logged and SHALL NOT block or terminate
  the worker (measurement only, never gating), and re-reading the gate on each heartbeat SHALL reflect a
  runtime halt within one interval.
  The worker SHALL instantiate the credential/identity engine (spec/016) at boot from operator config: a
  resolver composed of the optional native fallback plus every configured READ-ONLY credential source, each
  registered on its declared identity plane at its declared precedence (the machine-plane secret sources
  above the human-plane approver source). A source whose configuration is ABSENT SHALL be skipped (that
  capability is simply off); a source whose configuration is PARTIAL or invalid SHALL fail the worker boot
  CLOSED — a misconfigured credential source SHALL NOT silently drop and let a later actuation resolve a wrong
  or blank identity. The worker SHALL run an initial read-only sync of every configured source and, WHERE a
  sync interval is operator-declared, SHALL re-sync on that schedule; a sync error SHALL be logged and SHALL
  NOT terminate the worker, the source's prior converged state being retained (fail closed, never fall open).
  The worker SHALL, at boot, ESTABLISH the policy engine's out-of-box curated-default baseline per spec/015
  REQ-1523 — seeding the curated Semi-auto default ruleset AND its paired graduation defaults ONLY on a fresh
  deployment (absent-only; never clobbering an operator ruleset or an earned/operator-tuned op-class), a seed
  write failure being logged and tolerated (fail-closed), NEVER lifting the mode (mutation stays Shadow by
  default). The worker SHALL PUBLISH only the engine's NON-SECRET coverage and sync state — per-source plane, drift
  counts, last-synced, outcome, non-secret error text, and current target counts, NEVER a secret value — to a
  durable single-writer projection the console reads, best-effort exactly like the posture publish. This
  wiring resolves identities read-only; it SHALL NOT mutate the estate (mutation stays off).

- **REQ-1108** — [O] INV-10 · [R] readiness §4.A (the blind-verifier correctness fix).
  WHEN the execute activity builds the governed actuation request it SHALL supply a non-nil post-execution
  observer, so the deterministic verifier diffs the committed prediction against the real observed
  post-state and never a nil observation.

- **REQ-1109** — [O] INV-11.
  The execute activity SHALL populate the governed request's evidence by binding the proposal's cited
  tool-result ids against the investigation's orchestrator-captured read-only observations — captured,
  successful, recent, and target-relevant — so a mutating action citing no bound evidence is refused.

- **REQ-1110** — [O] TG-38 audit item R1 (the predecessor's GAP-1: pgvector provisioned, never used) ·
  [R] ADR-0003/ARCHITECTURE (the promised embedding retrieval, delivered).
  WHERE an embedding model is operator-configured (`TG_EMBED_MODEL`) and the durable vector index
  (migration 0013, `knowledge_embedding`, HNSW cosine) holds embedded precedent, the retrieval plane
  SHALL rank precedent by fusing the lexical channel with the query embedding's cosine top-K semantic
  channel using Reciprocal Rank Fusion (k=60) with a deterministic `external_ref` tie-break, SHALL
  exclude every semantic match whose cosine similarity is below the configured floor
  (`TG_EMBED_MIN_SIMILARITY`, default 0.5), and SHALL return the existing Hit shape (incident, score,
  human-readable reasons naming each channel's contribution) so the seed path is unchanged.

- **REQ-1111** — [O] INV-08 (model output is data; degrade honestly, never fabricate).
  IF no embedding model is configured, the durable store is absent, the index holds zero
  above-threshold embedded matches, or a query's embed or search call fails or times out, THEN the
  retriever SHALL return the lexical result unchanged for that query and SHALL log the degrade.
  Embedding writes SHALL be best-effort and bounded (the sync + backfill sweep, `TG_EMBED_BACKFILL_*`):
  an embedding failure SHALL leave the row's `embedding` NULL and SHALL NOT block a corpus write; a
  stored vector SHALL be computed only from the content-hashed precedent text it is keyed to (never
  fabricated, truncated, or padded); and a `TG_EMBED_DIM` differing from the migrated column dimension
  SHALL refuse worker boot with an error naming both values.

- **REQ-1120** — model-call observability at the gateway boundary. [O] INV-08/INV-13 · observe-only.
  EVERY model-gateway completion SHALL be observed exactly once at the boundary (`adapters/model`
  `Gateway.Complete`), independent of which caller made it (agent loop, offline admission gate, judge
  cron, skill generator, calibrator all share one gateway), so no model call is invisible. The gateway
  SHALL classify each call into a BOUNDED outcome (`ok | empty | rate_limit | timeout | bad_request |
  auth | provider_error | transport`) — carrying the HTTP status where one exists — and SHALL return a
  typed `*ModelError` (status + class) on failure so a caller can distinguish a transient failure from a
  permanent one; existing callers that only test `err != nil` are unaffected. A wired observer SHALL
  record the outcome + wall-clock into the observe registry (`tg_model_calls_total{model,outcome}` +
  `tg_model_call_seconds_total{model}` on `/metrics`, bounded-cardinality, secret-free) and SHALL log any
  non-ok or slow call as a structured line. Observation SHALL be side-effect-only: it SHALL NOT gate,
  retry, block, or fail a completion, and a nil observer SHALL leave behaviour identical (a 200 with empty
  content — a reasoning model that spent its budget thinking — SHALL be surfaced as `empty`, NOT an error).
  This observability grounded a latency finding that drives the composition-root wiring of
  `TG_SKILL_OFFLINE_MODEL` (default `fast`) for the skill-store offline pre-filter — see spec/014 REQ-1307.

- **REQ-1123** — per-domain actor-evidence observability. [O] INV-15 (publish the evidence, never assert
  it) · observe-only · companion to REQ-2307's advisory-per-reader rule.
  The actor-evidence fan-out reads a subject across EVERY armed domain reader and merges the results into
  one set, so an individual reader's contribution is invisible in the merged output. The system SHALL
  record, per domain: reads dispatched, evidence rows returned, and reads that ERRORED
  (`tg_actor_evidence_reads_total{domain}`, `tg_actor_evidence_rows_total{domain}`,
  `tg_actor_evidence_read_failures_total{domain}` on `/metrics`, bounded-cardinality, secret-free — a
  domain slug is not a principal). Three states SHALL remain distinguishable, because each has a different
  remedy: a reader NEVER ASKED (unwired), a reader asked that returned NOTHING (armed but out of scope),
  and a reader that FAILED (unreachable). Specifically: a zero-row read SHALL still increment reads and
  SHALL emit `rows=0` EXPLICITLY rather than omitting the series — an absent series and a zero series read
  identically in a graph and differently in an alert; and a failed read SHALL NOT increment reads, so an
  outage never renders as an empty scope. A process with NO armed reader SHALL emit no series at all.
  Observation SHALL be side-effect-only: it SHALL NOT gate, delay, or fail an evidence read, and a nil
  recorder SHALL leave behaviour identical.
  This exists because "N of 6 evidence readers have returned zero rows all-time" was carried as a
  per-reader finding while the system could not produce it — the only per-reader signal was the failure
  log, so a silent reader and a productive one were indistinguishable.

- **REQ-1112** — [O] INV-08 · design-wisdom #4 (ARCHITECTURE-DESIGN-WISDOM §3, backlog #4) — the
  trusted/untrusted seed boundary made machine-parseable.
  WHEN the Runner composes the agent seed it SHALL wrap every block in an explicit, consistent XML-style
  envelope named by KIND — `<summary>`, `<ticket>`, `<cmdb>`, `<precedent>` for the untrusted incident
  DATA and `<behavioral_guidance>` for the trusted guidance — and SHALL prepend a fixed preamble that
  names `<behavioral_guidance>` as the ONLY instructions and every other block as DATA the model reasons
  over but never obeys. BEFORE an untrusted block is wrapped, the Runner SHALL neutralize every envelope
  delimiter token embedded in that block's content (any opening or closing tag of any kind, matched
  case- and whitespace-insensitively), so a crafted alert, ticket, CMDB, or precedent body carrying a
  literal `</behavioral_guidance>` — or any envelope tag — SHALL NOT forge a block boundary: the composed
  seed SHALL carry exactly one real `<behavioral_guidance>` boundary. The neutralized content SHALL be
  retained, never dropped (an attacker SHALL NOT suppress triage by embedding a delimiter — under-triage
  is the worse failure). This envelope wrapping SHALL be ADDITIVE to the input screen: `core/screen` SHALL
  still run over each untrusted block, and a neutralized or truncated block SHALL be recorded in the
  session's skill_load seed provenance (REQ-1106). An untrusted block exceeding its per-block soft budget
  SHALL be truncated with a marker rather than dropped, so one oversized record cannot crowd the guidance
  out of the model's window. The grammar-validated identifier fields and the trusted guidance SHALL keep
  their existing handling; no untrusted token SHALL become an instruction (INV-08).

- **REQ-1113** — [O] INV-11/INV-19 · [F] spec/003 (BEH-3, the band-aware close-out lane) wired into the
  Runner · [R] Gulli ch12 (recovery must be reachable).
  WHEN a Runner workflow reaches a terminal outcome that SPENT a proposal (a completed proposal or a
  stood-down poll decision), the Runner SHALL drive the band-aware reconciler (core/reconcile) over the
  finished session through a dedicated terminal activity, and IF a tracker and the governance ledger are
  both wired THEN it SHALL transition the incident's tracker ticket and append the close-out decision to
  the hash-chained ledger. Ticket close-out SHALL be a TRACKER write (annotate/transition) only — it SHALL
  NOT actuate the estate and SHALL NOT be gated by the mutation chokepoint. The reconciler SHALL close an
  incident to Done ONLY on an orchestrator-confirmed clear under an auto band, and SHALL NEVER auto-close on
  a non-match verdict (a deviation/partial is routed To Verify — the never-auto floor); an unconfirmed or
  non-auto terminal SHALL leave the incident open (To Verify), never a silent close. The orchestrator-
  confirmed clear SHALL be PRODUCED by re-observing the live active-alert state of the incident's host over a
  BOUNDED RETRY WINDOW — re-observing periodically up to a maximum window so a recovery that clears LATER than
  a single poll cycle (e.g. a device-down alert that clears only after the host is re-polled UP) is still
  confirmed; a stale still-firing read simply triggers another retry and can never false-confirm — and
  reporting cleared only after CONSECUTIVE quiet readings (a debounce spanning the reader's poll cycle, so a
  FLAPPING host that is momentarily quiet then re-alerts resets and is never confirmed) where that host is
  QUIET, carrying NO active alert (so a host left in a WORSE state by a botched remediation, or an alert
  re-labelled by an unresolved rule name, is never read as cleared), failing closed (unconfirmed → To Verify)
  if the host is never quiet within the bound. It SHALL NOT be inferred from the mechanical verdict (a match EXCLUDES the
  target host's own alert, so it cannot mean the original condition cleared) and SHALL NEVER be sourced from
  the acting model's self-report (INV-11); it SHALL be produced ONLY for an action the Runner actually
  EXECUTED. It SHALL FAIL CLOSED on every unobservable path — no reader, a reader that could not fetch, or a
  blank signature ⇒ NOT cleared ⇒ hold To Verify — so a reader outage returning an empty observation is never
  mistaken for a clear. Within the bounded retry window, a captured provider RECOVERY transition for the
  incident host (a state-0 push the front door recorded durably at/after the ACTUATION INSTANT — the time
  captured immediately BEFORE the execute activity, NOT the later clear-confirm observation point, so a fast
  recovery pushed DURING post-actuation verification is still counted rather than excluded by the verify
  latency — TG's OWN evidence, never the model's word, INV-11) SHALL count as a quiet reading in the same
  consecutive-reading debounce, so a recovery that clears past the re-pull's reach is still confirmed; the belt SHALL fail closed
  (a nil recovery seam or a read error is NOT a clear) and SHALL be version-guarded so pre-existing histories
  replay deterministically. A confirmed-clear signal SHALL also arm the (host, alert_rule) novelty writeback so a
  verified-clean, confirmed-clear auto resolution de-novels its own incident shape for a hands-off
  recurrence. The writeback's clean-verdict precondition SHALL read the **FRESH per-execution verdict** —
  the mechanical verdict the interceptor's verifier computes against THIS session's post-state (the execute
  result) — NOT the durable per-action-shape verdict store: that store is content-addressed by action_id and
  append-only first-wins, so a re-cycled action shape otherwise inherits its FIRST execution's verdict forever
  (a stale partial/deviation would permanently block the de-novel of every later clean re-cycle; a stale match
  would false-authorize one that deviated). On a first execution the two are identical — except the two
  executed-but-unpersisted error tails (the manifest chain-gap and the verdict-persist-failure returns),
  where the fresh verdict now governs where no durable row existed; that direction is safe because
  deviation→never-auto is preserved and both tails trip the mutation breaker. The swap SHALL be
  version-guarded so pre-existing histories replay deterministically. The durable
  per-action-shape row remains the decision tracer's record of the action shape's FIRST verified outcome (TG-124).
  **SINGLE WRITER PER MEANING (roadmap P2-2, migration 0042).** The durable `action_verdict` store SHALL record
  EXECUTED actions only, and SHALL have exactly one writer: the actuation interceptor's verifier. The async
  falsifiability scorer, which grades committed blast-radius predictions for actions that were NEVER EXECUTED,
  SHALL write to the separate `prediction_verdict` store. Both stores keep the same append-only, first-wins,
  boundary-validated `Commit` contract, so the destination is a composition-root wiring decision rather than
  behaviour a caller can get wrong. *Rationale:* the two record different claims — "TG did X, did the estate
  respond as predicted?" (actuation accuracy) versus "TG predicted Y, did it happen?" (world-model accuracy) —
  and pooling them yields a rate describing neither. Measured live at the split: executed actions 85.7% match
  (24/28) against propose-path 44.9% (22/49), reported together as 59.7%; 23 of the 24 deviations were
  propose-path, so the pooled figure made a world model being wrong about an untouched estate read as TG
  mis-actuating. Legacy rows already in `action_verdict` SHALL NOT be relocated — it is append-only with
  UPDATE/DELETE revoked (migration 0015) — and SHALL instead be excluded by readers via the executed anti-join
  (an executed action always carries an `interceptor_gate_verdict` row with `gate='execute'` and
  `verdict='pass'`).
  The writeback (and the classifier's novelty read) SHALL key on the **incident subject host**
  (`env.Host`, the ingest-validated alerted device carried in `ClassifyInput.IncidentHost`), NOT the
  LLM-expressed action target — so the de-novel transfers to the next occurrence however that proposal
  expresses its target; the action target is retained only as a legacy compatibility read key (TG-124).
  The terminal activity
  SHALL be fail-safe: it SHALL NOT return an error (a close-out failure SHALL NOT fail a session that
  already reached its terminus), a nil tracker or ledger SHALL skip the close-out, and it SHALL be
  registered in the Runner's one canonical activity list.

- **REQ-1114** — [F] spec/003 (BEH-3, the reconcile requeue lane) wired into the worker · [R] Gulli ch12
  (recovery must be reachable) · [O] INV-01/INV-12.
  The worker SHALL schedule the escalation requeue lane's FireDue as a Temporal CRON workflow on a fixed
  cadence, so every DUE re-check in the escalation_queue is fired through the authenticated re-entry signal
  (core/escalation.Controller.FireDue) — an enqueued escalation SHALL re-escalate / page / stand down
  rather than sit in the queue forever. The cron SHALL be armed only WHERE a durable escalation store is
  configured (without one there is nowhere durable to enqueue). The FireDue activity SHALL be fail-safe: a
  FireDue error SHALL be captured in the run result and logged, SHALL NOT be propagated as a crash, and
  SHALL NOT crash or retry-storm the worker. The lane SHALL page humans and re-enter the gated pipeline
  only — it SHALL NOT actuate the estate (mutation stays off).

- **REQ-1115** — [O] INV-01/INV-12 · [F] spec/003 (BEH-3, REQ-206 the orphaned-poll requeue) wired into
  the Runner.
  WHEN the terminal reconciler flags a session as UNRESOLVED — an orphaned poll (a POLL_PAUSE that timed
  out unanswered) — the Runner SHALL hand the incident off to the escalation requeue lane for a delayed
  re-check, so an unresolved incident is re-examined against the live condition and converges to a human
  rather than being silently dropped. The hand-off SHALL be rate-capped by the escalation controller's
  per-incident cap (at the cap it stands down to a human). It SHALL be fail-safe: a nil hand-off seam SHALL
  skip the requeue (the close-out still records), and a hand-off error SHALL NOT fail the session terminus.
  The hand-off SHALL write ONLY the escalation queue — it SHALL NOT actuate the estate.

- **REQ-1116** — [O] INV-21 · design-wisdom #8 (ARCHITECTURE-DESIGN-WISDOM backlog #8; Gulli ch12 ·
  Anthropic 6.4) — bounded activity retries.
  The Runner workflow's ordinary read-only pipeline activities (suppress, classify, gate, notify,
  record-pending, resolve-pending, record-triage, reconcile, verify) SHALL run under a BOUNDED activity
  RetryPolicy — a finite MaximumAttempts (4) with capped exponential backoff (1s initial interval, 2.0
  backoff coefficient, 30s maximum interval) and a deterministic-error short-circuit (NonRetryableErrorTypes)
  — so a persistently-failing activity is retried at most MaximumAttempts times and then the failure
  SURFACES (the workflow ends as a failed session a human reconciles, or a discarded best-effort record
  returns and the session proceeds), and SHALL NOT be retried unboundedly under Temporal's default
  (MaximumAttempts 0). The read-only investigate activity SHALL run at most twice (one bounded retry over a
  transient model or tool blip, safe because the loop is read-only). The human-vote RECORD activity and the
  estate EXECUTE activity SHALL each be attempted at most ONCE and SHALL NOT be governed by the base bounded
  policy — a bounded retry SHALL NOT double-append the authorization ledger nor execute the estate twice. No
  Runner activity class SHALL run under an unbounded retry.

- **REQ-1117** — [O] INV-01 · design-wisdom #8 (Gulli ch12 · Anthropic 6.4 — a token/time budget stop) —
  the workflow wall-clock budget.
  The Runner workflow SHALL bound a single session's cumulative wall-clock — measured from workflow start
  via workflow.Now deltas — by a total-time budget, and WHILE a POLL_PAUSE session awaits the human vote it
  SHALL race a budget deadline against the vote wait. WHEN the session's wall-clock budget is exhausted
  before a decision arrives, the Runner SHALL STOP to the terminal orphaned-poll human-handoff — standing
  the session down fail-closed (deny by default, never a silent approval), recording the budget stop once on
  the hash-chained governance ledger, persisting the terminal triage record, and handing the incident off to
  the escalation re-check lane — and SHALL NOT crash and SHALL NOT mutate the estate. The budget SHALL be
  larger than the human-vote wait (VoteWait) so a decision within the poll window is never cut short. The
  budget-exceeded terminus SHALL be surfaced with a terminal outcome DISTINCT from a human timeout so a
  runaway session is audited, never silently swallowed. The budget stop SHALL reuse the existing
  orphaned-poll terminal path and SHALL NOT introduce a new terminal type or a new actuation. Any change to
  the workflow's command sequence introduced by the budget SHALL be guarded by a workflow version marker so
  in-flight histories replay deterministically.

- **REQ-1118** — [O] INV-12 · TG-126 · consumes spec/013 REQ-1218.
  WHEN the execute activity builds the governed actuation request it SHALL carry the CURRENT incident's
  classification band — the workflow's `decision.Band`, threaded as `ExecuteInput.Band` — so the interceptor's
  band-sensitive gates (the human-approval admission and the policy authorization) evaluate the fresh
  per-incident band rather than the reloaded sealed manifest's frozen first-seal band. The band SHALL be threaded as identifier data on `ExecuteInput` and SHALL NOT be
  re-serialized as authority; an absent band SHALL be `BandPollPause` (fail closed — it requires a recorded
  approval and never auto-admits). The reloaded manifest SHALL remain the authoritative sealed action for
  identity, argv, and prediction, unchanged.

- **REQ-1119** — [O] INV-08 · TG-124 deploy-persistence (the runtime corpus must survive a deploy).
  The retrieval plane's precedent corpus SHALL be read as the UNION of two files: a read-only bootstrap
  SEED (`TG_KNOWLEDGE_SEED_FILE` — tracked in git and re-synced onto the box by every deploy, so it is
  effectively read-only at runtime) and the MAINTAINED corpus (`TG_KNOWLEDGE_FILE` — untracked, so the
  deploy never touches it; the ONLY file the worker writes: novelty writeback, lessons merge, decay
  prune). Writing runtime precedent SHALL target the maintained file exclusively — pointing
  `TG_KNOWLEDGE_FILE` at the seed path makes every deploy silently wipe all runtime learning (observed
  live 2026-07-23). The union SHALL dedup by `external_ref` with the maintained record winning. A missing
  or not-yet-written maintained file SHALL NOT disarm the retrieval plane — the seed alone arms it
  (first boot before any writeback); a malformed MAINTAINED file SHALL keep the last good corpus (it is
  the write target — a torn write must never downgrade the retriever); a seed read failure SHALL degrade
  to maintained-only with a loud log, never hide accumulated runtime precedent; and a wholly-absent
  corpus (no seed configured AND no maintained file) SHALL preserve the no-retriever semantics (the
  novelty-gate-disabled warning fires) rather than mask the misconfiguration behind an empty retriever.

- **REQ-1121** — [O] INV-10/INV-19 · a verdict belongs to the execution that earned it.
  The workflow's verify step SHALL report a mechanical verdict ONLY for a session whose own execute step
  actually executed. WHERE this session executed nothing — refused at any gate, or a no-op — the verify step
  SHALL report no verdict and SHALL NOT count one on any observability surface, and the execution fact SHALL be
  threaded from the preceding execute result rather than inferred from the presence of a stored verdict.
  *Rationale:* the verdict store is keyed on the CONTENT-ADDRESSED action id and is first-wins, so two sessions
  producing the byte-identical action share one key — and repeated actions are the norm rather than the
  exception (measured: 113 executions collapsed into 28 durable outcomes). Reading the store by action id
  therefore answers "does a verdict exist for this action shape", NOT "did this session execute", and a session
  refused at the execute gate would republish an earlier session's `match` as its own and increment a counter
  documented as counting POST-EXECUTION verdicts. The reachable case is the one that matters: a target that
  refuses the command (spec/013 REQ-1220) writes no verdict of its own and would inherit a prior success.
  The threaded fact SHALL default to NOT-EXECUTED, so an absent value fails closed.

- **REQ-1122** — [O] INV-11/INV-19 · the clear-confirm belt answers for THIS incident, once.
  The lost-signal BELT that supplements the direct host-quiet observation SHALL be scoped to the incident's own
  `alert_rule`, and SHALL supply AT MOST ONE of the readings required to confirm a clear; the remaining
  reading(s) SHALL come from a direct server-side quiet observation. A belt read that cannot be scoped — an
  absent rule — SHALL return not-recovered rather than answer unscoped.
  *Rationale:* the belt read is MONOTONE. Its lower bound is pinned before the confirm loop begins, so once one
  qualifying recovery row exists it answers true on every subsequent tick. Unscoped, "did anything on this host
  recover" is then satisfied by an UNRELATED flapping rule while the incident's own rule is still firing, and
  because the answer never flips back it also defeats the re-alert debounce that flap protection depends on.
  Both properties this spec already claims — that ConfirmedClear requires an actual server-side quiet
  observation, and that a flapping host can never false-confirm because a re-alert resets the counter — are
  false while either hole is open. A false confirm is not merely a wrong row: it counts an unhealed incident
  into the published heal numerator, deflates the ineffective-actuation bound by the same value, auto-closes the
  ticket under an AUTO band, and writes a precedent that removes the first-sight human review from the NEXT
  occurrence of that (host, rule). The recovery record already carries the rule; it was simply never read.
  WHERE capping the belt changes the sequence of orchestration steps, the change SHALL be version-guarded so
  histories recorded under the previous behaviour continue to replay deterministically.

- **REQ-1223** — [O] INV-10/INV-11 · the clean run is asserted at the terminus, on re-observed facts.
  The session terminus SHALL feed the op-class graduation ladder exactly once for a session that EXECUTED a
  mutation, and the promoting CLEAN-RUN outcome SHALL require the same confirmed-clean facts the resolved-
  incident writeback requires: a terminal result, an answered poll, a mechanical verdict of `match`, and an
  orchestrator-confirmed clear. An executed session failing any of those SHALL be fed as NOT clean (breaking
  the consecutive-clean streak) rather than silently skipped; a session that executed NOTHING SHALL NOT touch
  the ladder in either direction; and an unattributable run — no op-class — SHALL write nothing rather than
  credit another class. A ladder write failure SHALL be best-effort and SHALL NOT fail the session.
  *Rationale:* the confirmed clear is the earliest signal in the session observed AFTER the monitoring surface
  has had time to refresh, which is exactly what the immediate post-execution verdict cannot be (spec/013
  REQ-1222). These same facts are already trusted to de-novel an incident and thereby remove a human's
  first-sight review on its next occurrence; evidence sufficient for that is the right evidence for autonomy,
  and using two different standards for the two decisions is the inconsistency this closes. WHERE no promote
  path is wired, that SHALL be surfaced loudly rather than left to present as "no op-class ever graduates".
  The "exactly once" above is a DISPATCH obligation, not only an arithmetic one: because the ladder write is
  not idempotent, it SHALL be dispatched from a unit of work permitted exactly ONE attempt, and SHALL NOT
  share a retrying unit with the rest of the terminus. A failure SHALL lose the credit rather than re-apply
  it. *Rationale:* a Temporal activity is AT-LEAST-ONCE, and `Ladder.Record` increments a bare counter with
  no dedupe key — so a retried terminus credits one confirmed heal once per attempt. Shipped 2026-07-27, the
  credit ran inside the terminal reconcile activity at `MaximumAttempts:4`, on a path whose tracker close-out
  had no HTTP client timeout: one heal could deliver FOUR of the five runs an op-class needs to reach AUTO.
  The estate was not over-promoted (the only class at AUTO earned it five days earlier, under the pinned
  path), but the earn path was live. The execute activity is pinned to one attempt for the same reason and
  says so; the requirement is written here so the earn path cannot drift back.

- **REQ-1124** — [O] INV-19 · the benchmark population counts what actually ran.
  The durable triage record SHALL reflect, at the session terminus, whether the session ACTUALLY executed an
  estate mutation. WHERE the triage row is written before the outcome is known — a session that pauses for a
  human approval records it at propose time — the executed fact SHALL be back-filled onto that row rather than
  left at its propose-time value. The back-fill SHALL be MONOTONIC (an execution cannot be un-run, so it only
  ever sets the flag TRUE), SHALL NOT set it for a session that executed nothing, and SHALL be best-effort:
  it feeds the offline scorer and re-enters no gate, so a persistence failure SHALL NOT fail a completed
  session.
  *Rationale:* the triage insert is first-write-wins, so the post-execute write is silently discarded and the
  row keeps `mutated = false` for an action that demonstrably ran. That flag is the A3 heal-rate DENOMINATOR
  and half the A7 ineffective-actuation bound, so it decides which incidents enter the published numbers.
  Measured when this was found: 94 executions recorded in the execution table against 91 triage rows marked
  mutated — and the three missing were not random. They were exactly the three heals on the lane that pauses
  for a vote, while every heal on the lane that executes straight through recorded correctly. The bias is
  therefore OP-CLASS CORRELATED: the benchmark counted the lane that never waits and dropped every lane that
  does, which is precisely the breadth evidence the actuation roadmap exists to accrue. A denominator that
  silently excludes a whole class of the thing being measured is not a small error.

- **REQ-1125** — [O] INV-10 · the runner supplies the verifier's durable baseline arm.
  The execute activity SHALL wire the actuation request's pre-anomalous reader to the durable ingest-ledger
  open-incident set (bounded by the ingest open-incident staleness) whenever a database pool is available,
  and SHALL leave it unwired otherwise — never a stand-in that always fails, so the interceptor's baseline
  gate can distinguish "arm unwired" from "arm wired but unreadable". The worker SHALL wire the same reader,
  anchored at LaunchedAt, into the deferred-verify channel (spec/017 REQ-1712a).
  *Rationale:* the baseline gate (spec/013 REQ-1224) is only as available as its arms; the durable arm is the
  one that does not share a failure mode with the live monitoring surface, so it is the wiring that keeps
  healing available during a monitoring blip while still refusing the manufactured-verdict class.

- **REQ-1126** — [O] INV-08 · corroboration counts OPEN incidents, not recent rows.
  The common-cause sibling corroboration SHALL ask which of the target's estate siblings hold an OPEN
  incident — a raise with no recovery, bounded by the ingest open-incident staleness — and SHALL NOT key on
  alert-row recency. The corroboration dep's signature SHALL NOT carry a recency parameter, so the
  row-recency definition is inexpressible without changing the type. All fail-open properties of the gate
  (nil deps, no siblings, lookup error ⇒ keep the alert-class decision) SHALL be preserved.
  *Rationale:* measured live 2026-07-28: 11 open incidents, 2 inside the 15-minute recency window —
  corroboration blind to 82% of the evidence it exists to weigh, so a real parent outage whose dependents
  dropped early reads as an isolated host-down. Incident-scoped suppression (spec/010) keeps first
  occurrences and drops repeats, which removes exactly the rows recency depends on (140 of 203 measured) —
  the open-incident definition is immune to that by construction, and it is the SAME semantics the actuation
  verifier's baseline arm uses (spec/013 REQ-1228), so corroboration and verification agree on what "already
  broken" means.
