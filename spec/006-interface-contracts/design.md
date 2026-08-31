<!-- spec/006 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/006 — Design: interface contracts

How the requirements in `requirements.md` are realized on the Go / Temporal / PostgreSQL stack. Where
this design and the code disagree, the code is the bug and this document is the intent. The predecessor
logic being re-expressed here is the n8n Runner/Bridge/Poller/receiver workflow set and
`scripts/lib/schema_version.py`; none of it is vendored — it is re-implemented under the safety core,
one grammar, and RBAC per [`docs/PORTING-GUIDE.md`](../../docs/PORTING-GUIDE.md).

## The authenticated HTTP surface (REQ-501, REQ-504, REQ-501b)

The ingress is the already-implemented `core/auth` router. Its two structural guarantees are the
mechanical realization of REQ-501: `Router.Handle` panics at boot on `AuthNone` (a forgotten auth
method yields a dead endpoint, never an open one), and a `PrincipalHandler` cannot run without a
verified `Principal`, which the middleware produces only after authenticating the caller and before the
handler reads the body. The HMAC path fails closed unless a bounded timestamp window and a nonce store
are both configured, which is how replay, stale-timestamp, and tampered-body rejection are enforced
without the handler having to remember to check.

The identity (`/v1/whoami`), stats, session-replay, governance-ledger, and connector-capabilities handlers
(`core/httpapi`) register on this router, and that same `Register` is the sole route source
`tools/gencontracts` walks — so the served surface, the generated contract, and the operator console
(spec/010) that consumes it cannot drift apart. `GET /v1/capabilities` is a read-only fleet-visibility view:
it returns the declared connector capabilities with enablement, so a disabled member (the Phase-0/1
actuation family, which has no execution path, INV-17) is visibly distinct from a live one and the
console/ops can never mistake a declared-but-inert capability for an available one. `/v1/whoami` echoes the authenticated principal and the live mutation posture; it homes in
`Register` (not inline at the composition root) precisely so it is generated into the contract like every
other route — a served-but-uncontracted route would violate the "the contract cannot drift from the served
surface" invariant the generator exists to hold (INV-15). The composition root (`cmd/grounder`) MUST call
`Register`: the surface is dead code unless mounted, so `buildPublicAPI` wires it and a route-walk oracle
asserts the whole set is served. `GET /v1/ledger` is a
pure read over the immutable, hash-chained governance ledger (INV-19): it returns the tail of the chain
(bounded page) in write order so the console can re-verify `prev_hash`/`hash` linkage client-side; it
never mutates and, with no durable ledger wired, fails closed to `503` rather than fabricating rows.
The read projection carries `created_at` — the storage clock for the row — so an operator surface can
show WHEN a decision was appended rather than rendering a blank column. It is projection metadata and
is NOT one of the hashed fields: the persisted chain over every historical row was computed from
`{seq, decision, reason, action_id, withheld, prev_hash}` alone, so admitting a timestamp into
`entryHash` would make `VerifyChain` report the entire untampered spine as broken. What protects the
timestamp is the no-UPDATE/DELETE privilege boundary (migration 0015), NOT the SHA-256 chain, and a
surface rendering it MUST NOT imply the stronger guarantee. The in-memory ledger has no storage clock,
so its entries carry the zero time and readers MUST treat that as "unknown", never as an epoch.
Session-replay is a read path plus a workflow-mint path: it never resumes a mutating session. A replay
request loads an immutable read-only `ContextSnapshot` and starts a fresh Temporal workflow that re-runs
ingest validation, the prediction gate, and the classifier from zero (closes H-01/P0-2). REQ-504 is realized
by an RBAC authority check plus a NOT NULL foreign key: a lookup the caller's role has no authority over returns zero rows for
both an unknown id and an unauthorized id, and the handler maps zero rows to a `404`, so the two cases
are observationally identical. REQ-501b is realized by `tools/gencontracts`, which walks the router's
registered routes and the typed Postgres entities to emit `openapi.yaml` / `asyncapi.yaml` / JSON
Schemas with a `generated_at`, source hash, and coverage scope; CI diffs the committed artifacts against
a fresh generation and fails on drift, an uncovered path, or a hand-written count.

The published SECURITY SCHEME is per-route and derived from the route's own auth method
(`auth.AuthMethod.SchemeName()`, recorded on `auth.RouteMethod` at registration), not a constant. It was a
hardcoded `tgHMAC` for all 57 routes while fifteen of them accept ONLY a browser operator session and 401
a valid HMAC credential — the repo's own `TestMachinePrincipalCannotSatisfyAdminRoute` asserts that 401,
so a green test contradicted the green, drift-gated contract and the drift gate held the contradiction
stable. A contract that tells an integrator to sign a request the server rejects is worse than none: it is
confidently wrong. The split published is the one the middleware enforces — an HMAC machine principal
satisfies HMAC / read-only / ingest-push and can never satisfy the session routes, which do not inspect a
signature at all (TG-249).

**mTLS publishes its own scheme, `tgMTLS`.** It is a machine credential but not the same one: an
`AuthMTLS` route 401s a valid HMAC signature, so publishing it as `tgHMAC` would reproduce the original
defect for whoever registers the first mTLS route. No route uses `AuthMTLS` today, which is why this was
latent — and why it was found by behaviour rather than by reading: the scheme mapping is now pinned by
driving a real signed request at a route registered with each auth method and requiring `SchemeName()` to
agree with what the router actually did. A table pairing method to scheme would be a second copy of the
claim, drifting alongside the first. The verifier below cannot catch this class: mislabelling every
session route as `tgHMAC` — the exact production defect — passes it, because it only proves the document
agrees with the model, never that the model agrees with the server.

Coverage verification checks properties a generator bug can violate: that each referenced scheme is
DEFINED under `components.securitySchemes`, and that it appears in THAT route's own security block. The
previous check substring-searched the whole document for the scheme name — a document the generator had
just written from that same string — so it verified only that the generator echoed its own input, and
returned nil even for a deliberately undefined scheme.

## The ingest event surface (REQ-502)

An alert enters through the authenticated front door `POST /v1/ingest/{source_type}` (`core/httpapi`): the
source authenticates by per-source HMAC, and the handler RESOLVES that vendor slug's ingester from the module
registry (`modules/resolve`). This is the load-bearing INV-17 gate on the entry path — an unregistered or
disabled ingest capability has no execution path, so the front door returns `404`; only a declared, enabled
source can inject an alert at all. The resolved ingester normalizes the raw payload against its explicit
grammar (INV-04): a payload that fails the grammar is rejected as a `400`, never coerced. A normalized
payload then MINTS the read-only Runner triage workflow, keyed by `external_ref` with reject-duplicate
semantics — so a re-fire of an in-flight incident is idempotent (it joins the running session, not a second
one), and a triage-backend failure is a `502` so the source retries rather than have its alert silently
dropped. The trigger is optional and graceful: a deployment with no Temporal wired accepts and normalizes
payloads without minting a session (validate-only), because the read-only API (`stats`/`ledger`/`whoami`)
must not depend on Temporal. Phase 0/1 stays read-only: the Runner drives to a gated proposal and stops; it
never mutates the estate.

Each ingest adapter (the reframe of a predecessor receiver workflow) normalizes its provider payload to
one canonical `Alert` shape and runs the deterministic `dedup → flap → burst → correlate` chain in code
before emitting anything. Flap is a CLUSTERING property: a dedup key is flapping only when ≥ `flapThreshold`
of its fires fall inside a single `flapWindow` span (a sliding window over the fires' timestamps), so
re-deliveries of one alert spread wider than the window never cluster into a false FLAPPING annotation —
counting raw whole-batch occurrences with no time bound was the port defect. Burst fires at ≥ `burstThreshold`
distinct correlated incidents; the threshold is **3** (the predecessor `BURST_THRESHOLD` and this repo's
own `ARCHITECTURE.md` "burst/correlation (3+ hosts)"), so a 3-host correlated group (e.g. Service up/down on
`pve01/02/03`) is recognized, not silently split into independent incidents. Publication is a
`triage.requested` event on the AsyncAPI-declared internal routing topic, keyed by `external_ref`; the
routing layer is a Temporal signal/queue, not a bare re-trigger. The correlation key is `external_ref`
because ids are unique within the organization's own trackers.

## The persistence surface (REQ-503, REQ-505, REQ-506, REQ-507)

`session_risk_audit` (REQ-503) is written by the classifier's Temporal activity as a required-field row
and appended to the hash-chained governance ledger; the acting model role holds no INSERT/UPDATE grant
on it. The `audit.Ledger` is CONCURRENCY-SAFE — the hash chain is inherently sequential (each row's seq +
prev_hash depend on its predecessor) and the ledger is shared across the worker's concurrent Temporal
activities, so `Append` (and the readers) serialize under a one-slot CHAIN GATE; without it concurrent
appends race and produce a non-monotonic, gap-broken chain with lost audit records. The gate is a channel
rather than a mutex, and the difference is LIVENESS, not correctness (TG-277): the gate is held ACROSS the
durable sink write, so when the substrate stalls a mutex would make every other governance decision in the
worker — classification, gating, mode transition, config and secret writes — wait without limit and without
recourse. `AppendContext` therefore bounds BOTH halves of that wait by the caller's deadline: queueing for
the gate (returning `ErrChainBusy`, which states that the append never happened, so nothing is recorded
that did not occur) and the durable write itself, via the optional `LedgerSinkContext` arm that hands the
deadline to pgx. An uncontended append takes a non-blocking fast path and is never refused for an
already-expired context. Plain `Append` remains, delegating on `context.Background()`, so every existing
caller is unchanged. The `audit.Ledger` computes the SHA-256
prev-row chain in process and, when a `LedgerSink` is
attached (the pgx-backed `db.LedgerStore`), mirrors every entry to `governance_ledger` write-through — a
sink failure fails the Append CLOSED (the chain never advances past an unpersisted decision). A restarted
worker continues the durable chain from its persisted tail (`audit.NewLedgerFromTail`), so the tamper-evident
audit trail is unbroken across restarts (INV-19); `VerifyChain` over the read-back rows detects any post-hoc
edit. The `governance_ledger` chain is ORG-GLOBAL, so more than one durable writer can share it — most
sharply the sibling worker that seeds the SAME persisted tail on a simultaneous deploy. A process's
in-memory tail is therefore only a CACHE of the real head, and a second writer can advance the head under
it: the losing INSERT then hits the `seq` PRIMARY KEY and the sink maps that unique-violation to the domain
sentinel `audit.ErrDuplicateSeq`. A seq collision MUST be RECOVERED, never fatal — on it the Ledger
re-reads the current head through the optional `LedgerTailReader` arm (the same `Tail` reader used to seed
the chain at boot), re-chains the entry onto that head, and retries, BOUNDED and honouring the caller's
deadline. Only the chain POSITION moves; the audit content is unchanged, so the single global chain still
verifies. This closes TG-549: before it, a boot-time collision failed closed permanently — the cached tail
never advanced, so every following append retried the same dead seq and the worker's whole governance lane
wedged until restart (and a simultaneous restart merely re-raced). A sink that cannot re-read the head
keeps the old fail-closed behaviour, and a head that never yields a free seq fails closed with a diagnostic
naming the contention rather than spinning without bound. The FULL de-identified `session_risk_audit` row (band, signals, plan_hash) is persisted through a
parallel `RiskAuditSink` (`db.RiskAuditStore`) attached to the same ledger, written BEFORE the ledger entry
so the detail is stored or the decision does not record — the DB `CHECK (auto_proceed_on_timeout = false)`
pins that invariant structurally regardless of the writer. `discovered_scheduled_reboots` (REQ-506) and `escalation_queue` (REQ-507) are the two other
governed tables this spec owns, each carrying `schema_version` and read under authority-checked RBAC. Their
in-memory oracle stores (`MemScheduledReboots`, `EscalationQueue`) are CONCURRENCY-SAFE — shared across the
worker's concurrent activities, they guard their map/slice with a mutex (the escalation seq is derived from
the slice length, so a concurrent enqueue would otherwise duplicate it). `MemScheduledReboots.Register` is
INSERT-OR-PRESERVE, keyed by `(host, kind, cron)` — the SAME identity as the pgx twin's PRIMARY KEY and the
predecessor's `uq_dsr_host_expr_kind` (cron is part of the identity, not a mutable attribute). Registering a
NEW `(host, kind, cron)` stores it in its supplied state, but re-registering an EXISTING one — a periodic
discovery sweep re-finding the SAME schedule — PRESERVES its promotion state (`State`, `Observations`,
`KillSwitch`) and refreshes only the validity window/schema. A sweep that finds a SHIFTED cron is a NEW,
unverified schedule that must observe before it suppresses (it does not inherit the old cron's promotion);
`Get(host, kind)` returns the most-recently first-registered cron, matching the pgx `ORDER BY created_at DESC
LIMIT 1`. This mirrors the predecessor's deliberate `ON CONFLICT (host,kind,cron) … DO UPDATE` (which does NOT
touch status/observed_count/kill_switch), so a sweep never silently demotes a schedule that promoted to live
nor clears an operator's kill switch, and keying on `(host, kind)` alone (which would carry a promotion onto
an unverified shifted time) is avoided; the
pgx twin matches (the state columns are omitted from its `DO UPDATE SET`, `RETURNING` the authoritative row). `EscalationQueue` exposes the
`escalation_queue` contract as three primitives — `Enqueue` (append a pending row), `DuePending` (the eligible
pending batch, oldest-first) and `MarkFired` (the append-only pending→fired transition, idempotent) — with the
SAME ctx-carrying signatures as its durable pgx twin (`db.EscalationStore`), so both satisfy the single
`escalation.Store` seam the spec/003 requeue controller drives and neither the queue nor the store holds the
re-entry signal (that is the controller's authenticated `SignalRequeue`). Schema
stamping (REQ-505) is a typed registry — the Go re-expression of the predecessor `schema_version.py`
`CURRENT_SCHEMA_VERSION` map and `check_row`: every writer stamps the current version for its table, and
every reader that decodes a structured column calls a `CheckRow` that returns a `SchemaVersionError`
when a row's stored version exceeds the reader's compiled version. The DDL, JSON Schema, and counts are
generated from the one typed entity per table (INV-15), so no parallel hand-maintained contract exists.
`knowledge_embedding` (migration 0013, spec/012 REQ-1110/REQ-1111 — the semantic-retrieval pgvector
sidecar) registers here at version 1 like every governed table: its writer (`db.KnowledgeEmbeddingStore`)
stamps rows via `schema.Stamp`, and the generated contracts pick it up from the same registry.
`session_triage` moved to version 2 with migration 0104 (TG-527): the row gains `trajectory`, the session's
ordered, digested tool path (`[]judge.TrajectoryStep` — tool + ArgsKey, never raw arguments) so the
trajectory_grounded axis can be scored over historical rows rather than only inside the eval harness's
process. The writer (`db.TriageStore.RecordTriage`) always writes the column non-NULL — `[]` for a session
that took no tool steps — so a NULL unambiguously means a pre-0104 row, which the scorer reads as N/A
rather than retro-grading; the version-2 stamp is what lets a reader tell the two row shapes apart.

## Decision procedure (per request)

1. The router authenticates the caller before the body is read; failure returns `401` before parse
   (REQ-501).
2. A route without an auth method never registers — the boot panics (REQ-501).
3. A replay request mints a new gated workflow from a read-only snapshot; it never resumes with
   caller-supplied input (REQ-501).
4. A lookup the caller's role has no authority over, or for an unknown id, returns `404` (REQ-504).
5. An ingest adapter normalizes, runs `dedup → flap → burst → correlate`, then publishes
   `triage.requested` keyed by `external_ref` (REQ-502).
6. A write to a governed table stamps `schema_version`; a read of a future-versioned row raises
   `SchemaVersionError` (REQ-505).

## Out of scope

The classifier that produces the `session_risk_audit` content is spec/001. The prediction gate is
spec/002. The reconciler and requeue firing logic that consume `escalation_queue` are spec/003. The
discovery and promotion writers that populate `discovered_scheduled_reboots` are spec/005. This spec
owns the contracts and the boundary, not the governed decisions behind them.

## Browser operator session (REQ-508)

The console (spec/010) needs a human-usable read path; machines keep HMAC/mTLS. The design adds THREE
auth-method values without touching the machine paths:

- **`AuthReadOnly` (route class)** — the pure-read surfaces (`/v1/whoami`, `/v1/stats`, `/v1/ledger`,
  `/v1/capabilities`) register this. The router wrap tries machine auth FIRST (identical strength and
  code path as `AuthHMAC`); only when no machine credential is present does it consider the session
  cookie, and a session principal is admitted for `GET` only (403 otherwise). `/v1/ingest/*` and
  `/v1/sessions/*/replay` stay `AuthHMAC` — a cookie is never even read there, so a browser session
  structurally cannot reach an action surface.
- **`AuthOperatorLogin` (route class)** — `POST /v1/session`. Authentication IS the credential check:
  the wrap verifies `X-TG-Operator` + `Authorization: Bearer` against the resolver's stored SHA-256
  digest (constant-time, rate-limited 5-failures/min per operator+ip) and mints the session before the
  handler runs; the handler only sets the cookie (INV-01's reject-before-parse holds).
- **`AuthSession` (route class + principal method)** — `POST /v1/session/logout` and the principal
  identity `operator:<name>`. The cookie is `<id>.<hex hmac-sha256(key,id)>`: signature proves the id
  was minted here, the server-side `SessionStore` makes revocation authoritative (logout deletes the
  row; a browser-held cookie is then worthless), and the TTL bounds the session's life. All three
  checks must pass — absent/tampered/expired/revoked are one indistinguishable `401`.

`core/auth/session.go` owns the machinery (`SessionAuthenticator`, `SessionStore` + in-memory store,
`OperatorResolver` + digest-only operators, the login limiter); `core/httpapi/session.go` owns the two
handlers. Composition (`cmd/grounder`): `TG_SESSION_KEY_REF` / `TG_OPERATOR_NAME` /
`TG_OPERATOR_TOKEN_REF` / `TG_SESSION_TTL` — all secrets as references (env:/file:), and if a
reference does not resolve the session surface is NOT registered (the browser path fails closed into
nonexistence; machine auth is unaffected). The nginx console proxy passes the cookie unchanged; the
cookie is `Secure` + `HttpOnly` + `SameSite=Strict`, so the read-only GET surface is CSRF-inert and
script-unreadable.

## Sessions read surface (REQ-509)

`GET /v1/sessions?limit=N` (AuthReadOnly) serves the console's session list from the AUDIT SPINE:
`core/db/sessions_read.go` selects the latest `session_risk_audit` row per `external_ref`
(`DISTINCT ON … ORDER BY created_at DESC`, bound `$1` limit) left-joined with **`action_execution`
(migration 0043) on `(action_id, external_ref)`** — the per-EXECUTION record of the deterministic
verifier's outcome (INV-10) — so a session's verdict is **the one from its own run**, and the handler
(`core/httpapi/sessions.go`) renders exactly that (empty list for an empty spine, 503 when the spine is
not wired; never fabricated rows, INV-15).

The join was previously against `action_verdict` on `action_id` alone. That is the SHAPE's ledger:
`action_id` is content-addressed over the operation (INV-07) and the table is keyed by it and written
first-wins, so one row is stamped onto every session that ever proposed the same operation. Live
2026-07-29 that put one verdict on three sessions at a time, and gave `match` to a session that never
executed at all. The verdict field therefore takes exactly one of:

| value | meaning |
|---|---|
| `match` / `partial` / `deviation` | this session executed and the verifier reached that verdict on ITS post-state |
| `unverifiable` | this session executed and the post-state could NOT be read (TG-182 fail-closed) — never to be read as a clean outcome |
| *empty* | this session did not execute, so it has no outcome of its own |

`unverifiable` and *empty* are distinct by contract: collapsing them would make "we acted and cannot
tell what happened" indistinguishable from "we did not act". The composition adapter
(`cmd/grounder` `sessionsReadStore`) decodes the stored signals jsonb; the oracle drives the handler
through the real router with an in-memory `SessionsReader` fake, both over an operator session and
unauthenticated.

## Alerts read surface (REQ-510)

`GET /v1/alerts?limit=N` (AuthReadOnly) serves the ingest tier's OWN record: `core/httpapi/alerts.go`
defines the `AlertLog` seam and the bounded in-memory ring (the Phase-1 store — the recent
accepted-envelope window since boot, which the console labels exactly that; a durable pgx twin joins
when the alert table lands). The ingest handler appends `recordFromEnvelope` (a projection of the
validated envelope + the minted triage workflow id) ONLY after acceptance — a grammar-rejected payload
never becomes a row (INV-04) — and the append can never block or fail ingest. Nil log = 503,
never fabricated rows (INV-15).

The page carries `counts` (`AlertCounts{total, last_24h}`) alongside the bounded row set, because the
page size is NOT a measurement: a surface given only `len(alerts)` reports the fetch limit, which read a
constant `50` on a store holding 1,553 accepted alerts. `Counts` is a separate read over the population
the page was drawn from, and it MUST fail closed exactly as `Recent` does — serving a zeroed count for a
store that could not be counted would tell an operator there are no alerts, which on an alerting surface
is the most dangerous available wrong answer. For the bounded in-memory ring `total` is what the ring
HOLDS (a since-boot window, never an all-time total); for the pgx twin it is the accepted-alert table.

`RecordFromEnvelope` is EXPORTED so that a caller which mints a triage WITHOUT traversing the HTTP front
door records the acceptance in exactly the same shape. The pve-liveness poller is such a caller: it detects
a stopped guest by polling Proxmox and starts the workflow directly, so before this it produced no
`ingest_alert` row at all — and A1 (detection recall) correlates injected faults against that table. TG's
FASTEST detector (~39s versus the ~6-11min push) therefore contributed ZERO to the metric it exists to
raise, and a guest-down it caught first scored as a MISS unless the slow path later pushed the same alert
and took the credit. Two constructors for one record would drift silently; there is ONE, and both callers
use it. The record is written only on a SUCCESSFUL mint — a detection that opened no investigation is not
an accepted alert — and the append never blocks or fails detection.

## Actions read surface (sealed ActionManifests)

`GET /v1/actions?limit=N` (AuthReadOnly) serves every sealed `ActionManifest` as the governed walk it
actually took: the manifest TG bound itself to before acting (op, op-class, target, reversibility, params —
INV-07), its band, its verdict, and five stage flags. It exists because the console's Actions surface
rendered FIVE INVENTED incidents against REAL estate hostnames while 109 genuine manifests sat unread; a
fabricated incident on a real device is worse than an empty panel, because an operator who believes it
investigates a machine that is fine.

Each stage flag MUST be grounded in a row that exists — `predicted` in a committed prediction hash,
`approved` in a recorded approval choice, `executed` in an `action_execution` row, `verified` in a written
verdict — never in a status guess. Absence is absence: an unscored manifest reports an EMPTY verdict and
`verified=false`, and MUST NOT render as a match. The execution probe is an `EXISTS` subquery, never a
JOIN: one manifest is ONE governed action however many times the effect leaf was attempted, and a JOIN
would multiply a retried action into several ribbons — the action-identity collapse that already turned 87
executions into 26 rows in this project's own reporting. The page carries `counts` (total, verified,
deviations) so the surface never presents its fetch limit as a population. A nil reader is 503, so the
console holds an honest empty state rather than falling back to the fixtures this endpoint retires
(INV-15).

The sessions page carries `total` — the spine's population, not the number returned — for the same reason
`/v1/alerts` carries `counts`: a bounded page fed to a surface as if it were a population reports the FETCH
LIMIT. The console's Knowledge model composes from this read at `limit=50` while the spine holds 1,306
sessions, so any count derived from it described the page. `SessionCount` MUST fail closed exactly as the
row read does — a zeroed total tells a surface the estate has no sessions when the truth is that nobody
could count them, which is a confident lie rather than an absence.

## Suppression shadow on the ingest front door

`Deps.Suppression` (a `SuppressionObserver`) observes each ACCEPTED alert and records whether
incident-scoped suppression (`core/ingest.DecideSuppress`) WOULD have dropped it as a repeat of a
still-open incident. It drops nothing. Nil is the OFF state, so a deployment without it ingests normally.

The interface takes NO context and returns NO error, and that shape is the guarantee rather than a
convenience: the handler has nothing to wait on and nothing to check, so an observation can neither delay
nor fail an acceptance. An implementation needing durable history MUST read it on its own goroutine with
its own deadline. A future signature carrying a `context.Context` or an `error` would make the front door
able to block on, or be failed by, a measurement — an oracle pins the shape so that change cannot land
silently.

Shadow-first is required rather than cautious. Measured on this estate, 73.3% of organic alerts are repeats
(400 alerts across 107 keys over 7 days), so the upside is real; but a suppressor wrong in the other
direction produces an incident nobody sees, and a time-windowed design would additionally drop the FIRST
alert of every re-injection and collapse A1. The judgement therefore earns its way in on live evidence
before it is permitted to act — the same discipline TG applies to its own actuations: commit the
prediction, then score it.

## Governance + secret-reference surfaces (REQ-511, REQ-512)

`GET /v1/governance` (AuthReadOnly) composes the posture from the components that OWN each fact:
`safety.MutationGate` (enabled/preflight), `db.SessionReadStore.BandCounts` (the spine's band
distribution, one bound query), `db.LedgerStore.Tail` (chain head). `GET /v1/secrets` lists the
configured `config.SecretRef` values with a per-request resolution probe whose value is discarded
immediately — `httpapi.SecretRefStatus` has no value field, so a secret cannot be serialized onto the
surface by construction. Both handlers are GET-only and 503-fail-closed on a nil reader (INV-15).

## Liveness stream (REQ-513)

`GET /v1/events` (AuthReadOnly) is `text/event-stream`: an immediate `posture` event on connect (the
`GovernanceReader` snapshot, same assembly as REQ-511), then one per `Deps.EventsInterval` (default
5s) until the client disconnects. Emit-only; `X-Accel-Buffering: no` keeps the console's nginx from
buffering. Nil reader or a non-flushable writer = 503 (INV-15: the console's live dot reflects a real
stream, never a client-side simulation).

## Models read surface (REQ-514)

`GET /v1/models` (AuthReadOnly) relays the LiteLLM control response verbatim: the composition
(`cmd/grounder/models_read.go`) calls `<gateway>/model/info` with the master key resolved per request
and discarded (the key never reaches the client), and the handler (`core/httpapi/models.go`) writes
the gateway's bytes unmodified. Nil reader / unreachable gateway / non-200 / empty body = 503 —
never a fabricated fleet (INV-15).

## Contract read surface (REQ-515)

`GET /v1/contract` (AuthReadOnly) serves the generated OpenAPI verbatim: `docs/contracts/embed.go`
embeds the repo artifact (`go:embed openapi.yaml`), the handler writes it unmodified
(`application/yaml`, 503 when empty). Honesty is inherited from REQ-501b's drift gate —
`gencontracts -check` fails CI whenever the committed artifact differs from the registered route
table, so the endpoint map this surface serves provably matches the endpoints it is served from.
The console renders it natively as the "API" view (no vendored Swagger UI, no CDN).

## Estate read surface (REQ-516)

`GET /v1/estate` (AuthReadOnly) serves the causal estate graph the WORKER builds — the grounder never
builds it. The worker (spec/012) writes a snapshot after each `estate.Build`/`Holder.Refresh` into the
schema-stamped `estate_snapshot` table (migration 0005); the grounder's `db.EstateReadStore` reads the
latest row (one bound query, `ORDER BY captured_at DESC LIMIT 1`) and decodes the graph projection
(`estate.Graph.Export` → nodes + confidence-weighted edges). The handler (`core/httpapi/estate.go`)
serves it, reports `available:false` when no snapshot exists yet, and 503s when the store is not wired
— never a fabricated topology (INV-15). The console renders it as the Estate view's LIVE overlay.

## Credentials read surface (REQ-526)

Three AuthReadOnly GET routes publish the credential engine's (spec/016, TG-107/TG-89) REAL persisted
state for the P1 credential console (TG-109) — the grounder reads, the WORKER writes. `db.CredentialReadStore`
runs the queries (all parameterized `$1`/`$2`, selecting ONLY the non-secret columns the tables hold):

- **`GET /v1/credentials/sources`** — the latest `credential_sync_run` per source (`DISTINCT ON (source_id)
  … ORDER BY started_at DESC`, migration 0017) LEFT JOINed to the current `credential_coverage` count,
  ordered plane then source_id: plane, last-synced, drift (added/changed/removed), covered-target count,
  outcome. `precedence` is NOT persisted (it is worker config), so it is honestly OMITTED, never fabricated.
- **`GET /v1/credentials/resolutions?target=&limit=`** — the recent `credential_resolution` audit tail
  newest-first (migration 0018), `?target=` filtering to one target, `?limit=` clamped to 200 (default 50):
  target/plane/outcome/source/native/rule/resolved-user/scheme/**key-ref-scheme**/shadowed/error/created-at.
- **`GET /v1/credentials/coverage`** — a summary DERIVED from the recent (30-day) resolution window:
  resolved/unresolved/ambiguous tallies per plane and per source (GROUP BY), plus each distinct target's
  most-recent resolved-vs-refused outcome (`DISTINCT ON (target)`) as the coverage frontier.

**No secret can leak (INV-13):** the `credential_sync_run` / `credential_coverage` / `credential_resolution`
tables are secret-free by construction — a source stores REFERENCES, never values — so the most any response
carries about a secret is a key reference's SCHEME (`env`/`file`/`store`/`vault`/`bao`). The read store selects
no other column; the DTOs (`core/httpapi/credentials.go`) have no field that could receive key material, a
`SecretRef` value/path, or a token; an oracle walks the serialized JSON and fails on any forbidden field name.

**Not a live probe (documented follow-up):** coverage answers "what can TG currently reach?" from PERSISTED
resolution outcomes only. A live "can TG reach host X now?" resolve-probe would need the worker-side
SyncEngine/resolver (which lives in the worker, not the read-only grounder) or a persisted entry set; that is
an explicit FOLLOW-UP, not this read surface. The history-derived view is honest and real.

## Control-plane self-config: the admin tier, ledgered writes, sealed secrets (REQ-520/522/523/524, task #27)

The signed-off #27 design thesis: console-native config is a security UPGRADE over SSH/.env — RBAC-
gated, ledgered, revocable — IFF config/secret writes stay strictly DISJOINT from estate mutation.
Nothing here touches the actuation adapter, the never-auto floor, or the mutation switch; the LAW
keys (`safety.*`) are exactly the ones every layer refuses to write.

**Read (REQ-520).** `core/cpconfig` is the compiled registry (which knobs exist, which are LAW,
which are console-writable) + the layered resolver: console override (only for a console-writable
non-LAW key) → env → compiled default, with a LAW key ALWAYS resolving to its compiled value.
`GET /v1/config` (AuthReadOnly, `core/httpapi/config.go`) serves each knob's value + source; nil
resolver ⇒ 503; no secret value exists in the registry by construction.

**Admin tier (REQ-522).** Two new route-only auth classes in `core/auth`: `AuthAdminElevate`
(step-up: a VALID session cookie + the separate admin credential in `X-TG-Admin` +
`Authorization: Bearer`, verified constant-time and rate-limited, marks the server-side session id
admin-elevated for a short TTL — `core/auth/admin.go` `AdminAuthenticator`) and `AuthAdminSession`
(admits only a valid session with a live elevation; the principal carries `Admin=true`; HMAC/mTLS
material is never inspected). Composition: `TG_ADMIN_NAME` + `TG_ADMIN_TOKEN_REF` + `TG_ADMIN_TTL`
(default 15m); an unresolvable admin token means `buildAdminSessions` returns nil and the
`d.AdminSessions != nil` block in `httpapi.Register` never registers `/v1/session/elevate`,
`/v1/config/{key}`, or `/v1/secrets/{name}` — the admin lane fails closed into nonexistence.
Elevation is in-process state with a short TTL: a grounder restart drops it (re-elevate), a session
revocation orphans it harmlessly (a cookie that fails verification never reaches the elevation
check).

**Config writes (REQ-523).** `POST /v1/config/{key}` (`core/httpapi/config_write.go`) validates
fast (`cpconfig.ValidateWrite`: registered / non-LAW / console-writable / bounded value ⇒
404/422/422/400) and delegates to the WORKER via `temporal/configwrite.ConfigWriteWorkflow`
(distinctly named — the bare-name collision guard in `temporal/skilltrial/finalizer_names_test.go`
lists it). The activity re-validates (the authority), appends `config:set` to the SAME durable
hash-chained governance ledger the worker owns, THEN upserts `control_plane_config` (migration
0011, schema-registry `control_plane_config`) — ledger-before-commit, mirroring the skill-store
discipline. The resolver reads the store as its ConsoleStore; `applyConfigOverrides` (cmd/grounder)
re-adopts legal overrides into the boot config so the /v1/config report and the running components
agree (INV-15).

**Sealed secrets (REQ-524).** `core/seal`: envelope encryption — AES-256-GCM under a fresh
per-secret DEK, DEK wrapped by the master key (`TG_SEAL_KEY_REF`, resolved per use and discarded),
both AEAD-bound to the secret NAME. `POST /v1/secrets/{name}` seals IN THE GROUNDER; only
ciphertext enters `temporal/configwrite.SecretPutWorkflow` (Temporal history holds no plaintext),
which ledgers `secret:put` (name + ciphertext digest) then upserts `sealed_secret` (migration
0012). **Every step of both governed writes is separately BOUNDED and MEASURED (TG-277).** Each of
ledger append, schema stamp and store write runs under its own `DefaultStepBudget` (4s), and the
three budgets are required to fit inside the activity's 15s `StartToCloseTimeout` so a stalled step
always names itself before Temporal's opaque timeout fires — under `MaximumAttempts: 1` the operator
gets exactly one message and it has to identify the cause. A `LatencyObserver` reports every write's
per-step timing, on success and failure alike, wired to the worker's log. The timeout itself is
UNCHANGED and correct: measured live on dc1tg01 2026-08-04 against a ledger at seq ~8800, six
consecutive sealed-secret writes completed the whole workflow in 40-43ms with the activity at
11-13ms. The ~10.5s that prompted the investigation was a Temporal WORKFLOW-task timeout during a
substrate stall, not this activity's work — which is exactly why the per-step record exists.
Consumption is the new `store:<name>` scheme on `config.SecretRef.Resolve()` — wired at
composition only when the master key resolves, else fail-closed-unwired. The read side extends
`GET /v1/secrets` with a `sealed` section (name, `store:` ref, purpose, timestamps) whose DTO has
no value field; the write response carries the reference and ledger seq, never the value.

**Console.** The `#secrets` view (deploy/console/v2 `_live` layer): writable non-LAW rows gain an
inline editor (value + mandatory rationale, save disabled until both), LAW rows render "pinned by
law", the sealed-secret form is write-only (the value input is cleared the moment the request
completes), and the honest-state ladder mirrors the skills editor (401→elevation modal, 422→"the
clamp is the law", 404/405/503→"write path not deployed", 429→rate limited).

## Durable operator sessions (REQ-508 durability)

The browser session store is Postgres-backed (`db.SessionStore`, table `operator_sessions`, migration
0006) so a valid session survives grounder restarts/redeploys — the in-memory store wiped every session
on restart, forcing a re-login on each deploy. The security model is unchanged: the cookie is still the
signed `<id>.<hmac>`, only the id→(operator,expiry) mapping is persisted, and logout stays authoritative
(`Revoke` deletes the row). The in-memory store remains the CI oracle for the `SessionStore` seam; the
pgx store is its durable twin (integration-tested under compose, compile-time interface-asserted).

## Verdict-ledger provenance signature (2026-08-22, TG-81 b3)

`action_verdict` gains schema version 2 (migration 0108): a `signature` column carrying an Ed25519
signature over the canonical verdict tuple (`action_id`, `plan_hash`, `verdict`, `target_host`,
`site`), rendered by `core/verdictsig` — clean-room from the h-network RFC-ASA signed-verdict
envelope, with Ed25519 over their HMAC so the triage plane verifies with only the PUBLIC key and
never holds the key that could mint a verdict. The deterministic verifier stays the sole verdict
AUTHORITY (INV-10); the signature adds provenance only: the prior-verdict reader
(`db.PriorVerdictStore.WithVerifier`) drops a SIGNED row that fails verification — a row INSERTed
around the interceptor's VerdictSink — treating it as an absent verdict (evidence removed, review
raised), while unsigned rows ('' — all pre-0108 history and unarmed-writer rows) stay evidence.
Both halves are dormant by default: `TG_VERDICT_SIGNING_SEED_REF` (secret ref, actuation plane) arms
the writer, `TG_VERDICT_PUBLIC_KEY` (hex, not a secret) arms the reader; unset means byte-identical
rows and reads, and a set-but-unresolvable seed refuses the boot rather than booting an unsigned
writer that looks armed.

## A lifecycle label names its session (2026-08-22, TG-532)

`action_manifest` gains schema version 2 (migration 0110): `approval_ref` and `verdict_ref`, the session
each non-hashed lifecycle label describes. The seal is per-SHAPE by construction — `action_id` is
content-addressed (INV-07) and the row is first-wins — so one manifest is the identity of every session
that proposed that operation: measured on production 2026-08-22, 69 action_ids are shared by more than
one session and the worst by 198. Writing per-SESSION facts (a human's approval choice, one run's
mechanical verdict) onto that row last-write-wins made the tracer and the `#actions` ribbon answer
"which session?" with silence, which a reader fills with the session in front of them — the filed case
read `approved/match` from a row sealed a month earlier. Each `*_ref` moves with, and only with, its own
label (the CASE keys on the same NULL the COALESCE does), an unknown session writes `''` rather than
inheriting the previous owner, and the ribbon additionally publishes `sessions_sharing` so a shape's
history can never be mistaken for a session's outcome. Nothing in the content hash changes; the labels
remain observability-only.
