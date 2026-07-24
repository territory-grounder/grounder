<!-- spec/006 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/006 — Interface contracts

**Owning behavior family:** BEH-6 (see [`docs/GOVERNED-BEHAVIORS.md`](../../docs/GOVERNED-BEHAVIORS.md)).
**Constitution / invariants:** INV-01, INV-15, INV-16, INV-19.
**Phase:** the mandatory auth middleware lands in Phase 0 (`core/auth`, already implemented); the
event, persistence, and generated-contract surfaces land in Phase 2–3. **Status:** Approved.

This spec owns the boundary between the outside estate and the governed spine: the single
authenticated Go router, the internal `triage.requested` routing event, the persistence contracts the
governed behaviors depend on (`session_risk_audit`, `discovered_scheduled_reboots`, `escalation_queue`),
`schema_version` stamping, and the generated wire contracts (`openapi.yaml` / `asyncapi.yaml` / JSON
Schemas). The predecessor exported roughly 25 unauthenticated webhooks and hand-maintained parallel
contracts; every requirement below closes one of those classes. The requirement source of record is
this file; the design is in `design.md`, the runnable acceptance oracles are in `acceptance/`, and the
engineering tasks are in `tasks.json`.

## Requirements

- **REQ-501** — [F] spec/006 · [O] INV-01.
  The system SHALL accept **stats** and **session-replay** requests over HTTP only through the
  mandatory, non-bypassable authentication middleware — mTLS client certificate, or per-source HMAC
  over the raw body with a timestamp and a nonce replay window — SHALL reject an unauthenticated
  request before body-parse, and SHALL refuse at boot to register any route declared with `auth=none`.
  A "session-replay" re-engagement SHALL mint a **new** Temporal workflow that re-runs the full gate
  from zero, seeded only by an immutable read-only `ContextSnapshot`, and SHALL NOT resume a mutating
  session with caller-supplied input (there is no privileged resume-with-prompt primitive; closes
  H-01/P0-2).

- **REQ-501b** — [O] INV-15 (overlay-added binding on the interface surface).
  The machine-readable `openapi.yaml`, `asyncapi.yaml`, and JSON Schemas SHALL be **generated** from
  the canonical Go/Postgres model, SHALL cover every routed endpoint with declared auth, error, and
  idempotency schemas, and SHALL embed a non-null `generated_at`, source hash, and coverage scope; CI
  SHALL fail on a hand-written count, an uncovered path, a drift, or a missing provenance field. This
  generated contract is the single contract the `frontend/` console and every module consume.

- **REQ-502** — [F] spec/006 · [R] paradigm-rule 3.
  WHEN an ingest module (receiver) fires, the system SHALL normalize the alert to one canonical shape,
  run `dedup → flap → burst → correlate` in code before publishing, and SHALL publish a
  **`triage.requested`** event to the routing layer keyed by `external_ref`.

- **REQ-503** — [F] spec/006 · [O] INV-19.
  The system SHALL guarantee the `session_risk_audit` persistence contract: exactly one structured,
  required decision record per classification (BEH-1), appended to the tamper-evident hash-chained
  governance ledger.

- **REQ-504** — [F] spec/006.
  WHEN a replay or stats lookup targets an unknown id, or an id the caller's role has no authority over, the
  system SHALL return **not-found**, so an unauthorized id is indistinguishable from a missing one (an RBAC
  authority check plus a NOT NULL foreign key make an unauthorized read unresolvable).

- **REQ-505** — [F] spec/006 · [O] INV-15/INV-16.
  The system SHALL stamp `schema_version` on every governed row against a canonical registry, its
  readers SHALL reject a row written by a future version with a `SchemaVersionError` rather than
  mis-reading it, and the DDL, JSON Schema, validators, and human-facing counts SHALL be generated from
  one typed source per entity with no hand-maintained parallel contract.

- **REQ-506** — [F] spec/006.
  The system SHALL guarantee the `discovered_scheduled_reboots` persistence contract (BEH-5): a
  bi-temporal schedule registry row carrying `kind`, `cron`, an `observing`/`live` state, a
  `kill_switch`, and `valid_until`, each row stamped with `schema_version`.

- **REQ-507** — [F] spec/006.
  The system SHALL guarantee the `escalation_queue` persistence contract (BEH-3): an append-only,
  rate-capped requeue lane whose rows re-enter the gated pipeline as an authenticated internal Temporal
  signal keyed by `session_id`, each row stamped with `schema_version`.

- **REQ-508** — [R] spec/010 consumer · [O] INV-01 (mandatory auth, default-deny).
  The system SHALL provide a browser operator session as a third authentication method beside HMAC and
  mTLS, without weakening either: WHEN an operator presents a valid operator name + bearer token (both
  supplied via secret references, with only a SHA-256 digest of the token held in process) to the
  session login route, the system SHALL mint a server-side-registered, HMAC-signed, HttpOnly,
  SameSite=Strict session cookie with a bounded TTL; WHEN a request carries a valid session cookie on a
  route registered read-only the router SHALL produce a read-only session principal for safe (GET)
  requests ONLY; and a session principal SHALL never satisfy an HMAC or mTLS route (those routes never
  inspect a cookie), so ingest, replay, and every future mutation surface remain machine-only. An
  absent, tampered, expired, or revoked cookie SHALL be rejected before the handler runs,
  observationally identical to any other unauthenticated request; logout SHALL revoke the server-side
  session authoritatively; login attempts SHALL be rate-limited per (operator, source-ip) and compared
  in constant time; and an unconfigured session authenticator SHALL mean the browser path does not
  exist at all (fail closed).
  The system SHALL additionally support a directory-backed login composed WITH, and never weakening,
  the static operator+token: WHEN LDAP/FreeIPA login is configured and the presented name is NOT the
  static break-glass operator, the system SHALL authenticate by binding AS THE USER against the
  configured directory replica(s) over verified TLS (LDAPS or StartTLS; the server certificate verified
  against a configured CA, never skipped) — the presented bearer value being the user's OWN directory
  password, which the system SHALL never store, hash, or log (the directory verifies it; a successful
  bind IS the identity proof) — then read that user's OWN group membership and map it to a role: a
  member of the configured admin group SHALL yield an admin-eligible session, a member of the configured
  operator group SHALL yield a read-only operator session, and a bound user in NEITHER allowed group
  SHALL be DENIED (fail closed — a valid directory user with no grantable group cannot log in). A bind
  failure, an unreadable group set, or all replicas being unreachable SHALL fail closed as one
  indistinguishable unauthenticated response, and an LDAP failure SHALL NEVER fall through to the static
  token path. The static break-glass operator SHALL always use the constant-time token path so an
  unreachable or misconfigured directory can never lock every operator out.

- **REQ-509** — [R] spec/010 consumer · [O] INV-10/INV-15 (the verifier is the sole verdict author;
  no fabricated contract surface).
  The system SHALL serve a read-only sessions surface from the governed session spine: the latest
  governed session per external_ref — its classification (band, risk level, the content-hashed action
  id, plan hash, auto/notify/override flags, and the driving signals) when one was sealed, ELSE the
  investigation/triage record (so an agent run that reasoned and STOPPED, leaving a triage row but no
  sealed classification, still surfaces rather than vanishing) — joined with the deterministic
  verifier's verdict for the bound action, empty when no verdict exists; classification-only fields are
  empty (never fabricated) for a triage-only session. The surface SHALL be GET-only, bounded (paged),
  authenticated like every read route (machine principals or a read-only operator session), and SHALL
  fail closed to unavailable when the durable spine is not wired rather than fabricate rows.

- **REQ-510** — [R] spec/010 consumer · [O] INV-04/INV-15 (grammar-validated envelopes; no fabricated rows).
  The system SHALL serve a read-only alerts surface from the ingest tier's own record: every envelope
  the alert front door ACCEPTED (normalized, grammar-validated) is appended — with its source type,
  validated fields, and the triage workflow id it minted — to a bounded alert log at the moment of
  acceptance; a rejected payload never appears (it never became an envelope). The surface SHALL be
  GET-only, bounded (paged), newest-first, authenticated like every read route, SHALL fail closed to
  unavailable when no log is wired, and appending SHALL never block or fail the ingest path.
  An accepted envelope carrying a provider RECOVERY-transition label (a fault's alert went back UP) is NOT a
  new incident: the front door SHALL NOT mint a triage session for it, SHALL capture it to a durable
  recovery-transition log as clear-evidence for the waiting Runner (spec/012), and SHALL still return
  accepted; capture SHALL never block or fail the ingest path, and a nil recorder SHALL route recoveries as
  before (fail-safe inert).

- **REQ-511** — [R] spec/010 consumer · [O] INV-15 (posture read from the authoritative components).
  The system SHALL serve a read-only governance surface assembled from the authoritative components
  themselves: the audit spine's band distribution (latest classification per external_ref, grouped), the
  governance ledger's chain head, and the mutation posture — GET-only, authenticated like every read route,
  failing closed to unavailable when a component is not wired.
  The reported `mutation_enabled` (on this governance surface AND on the identity/stats surface) SHALL be the
  WORKER's published live posture, read across the process boundary — the authoritative mutation gate lives
  in the worker process, so the read-only grounder's own gate SHALL NOT be reported as the mutation state.
  WHEN the worker's published posture row is older than a bounded freshness threshold, or is absent, the
  surface SHALL flag the posture as stale/unknown (a `posture_stale` boolean and a `posture_source` label)
  and SHALL report the freshest reading it holds rather than a confident false OFF, so a heartbeat gap can
  never advertise a read-only posture for a worker that can act. The surface SHALL also carry the worker's
  effect-leaf `effect_capability`; the `preflight_green` bit stays the local gate's own.

- **REQ-512** — [R] spec/010 consumer · guardrail "no literal secrets".
  The system SHALL serve a read-only secret-REFERENCE surface: each configured reference (env:/file:),
  its purpose, and whether it currently resolves — and the response type SHALL carry no value field at
  all, so a secret value cannot be serialized onto this surface by construction. GET-only,
  authenticated, fail-closed.

- **REQ-513** — [R] spec/010 consumer · [O] INV-15 (a live indicator must reflect an actual stream).
  The system SHALL serve a read-only liveness stream: Server-Sent Events carrying the governance
  posture (the same assembly as the governance surface) — an immediate snapshot on connect, then one
  per bounded interval — authenticated like every read route, emitting state and accepting nothing,
  and failing closed to unavailable when the posture reader is not wired.

- **REQ-514** — [R] spec/010 consumer · [O] INV-15 (relay the gateway's own report, never a summary).
  The system SHALL serve a read-only models surface as a verbatim passthrough of the model gateway's
  control response (model inventory/usage), fetched server-side with the gateway key resolved per
  request and discarded — the key SHALL never reach the client. GET-only, authenticated like every
  read route; a nil reader, unreachable gateway, non-200 answer, or empty body SHALL fail closed to
  unavailable rather than fabricate a fleet.

- **REQ-515** — [R] spec/010 consumer · [O] INV-15/REQ-501b (the map provably matches the territory).
  The system SHALL serve the generated wire contract verbatim on an authenticated read route: the
  embedded artifact is the repo's generated OpenAPI document, which the existing contract drift gate
  already proves equal to the registered route table — so the served endpoint map cannot diverge from
  the served endpoints. GET-only; an empty document SHALL fail closed to unavailable.

- **REQ-516** — [R] spec/010 consumer · [O] INV-15 (serve the worker's graph, never a grounder-built copy).
  The system SHALL serve a read-only estate surface: the latest published snapshot of the causal estate
  graph — the confidence-weighted dependency edges the worker builds and the prediction gate reasons
  over — as its node set, edge set, and capture time. The grounder SHALL NOT build the graph; it SHALL
  serve the latest worker-published `estate_snapshot` row (each row schema-version stamped, REQ-505),
  reporting available=false when no snapshot exists yet and failing closed to unavailable when the store
  is not wired — never a fabricated topology.
- **REQ-517** — [R] spec/010 consumer · [O] INV-15, INV-22 (publish the differentiator as evidence, never
  assert it). The system SHALL serve a read-only **grounding scorecard**: live aggregates over the
  authoritative verdict, prediction, and audit tables that make TG's committed-prediction +
  mechanical-verifier loop *falsifiable in public*. It SHALL report the mechanical verifier's
  match/partial/deviation distribution and match-rate (`action_verdict`), the blast-radius
  precision/recall, the falsifiability signal — average real true-positives versus the
  degree-preserving shuffled-graph control's true-positives, INV-22 (`infragraph_prediction`) — the mean
  blast-radius **false-positives per scored prediction** (the over-prediction rate `sum(fp)/predictions`,
  the honest view precision cannot express: a correctly-restrained `n_pred=0` true-negative lowers it and
  it self-heals as calibrated predictions accumulate), and the
  autonomy-band distribution with the never-auto floor-hold count (`session_risk_audit`). Every figure
  SHALL be a computed aggregate over real rows; an empty spine SHALL report zeros (a checked divisor,
  never a fabricated or NaN rate), and the surface SHALL fail closed to unavailable when the store is not
  wired — the scorecard never invents a match-rate it cannot substantiate.
- **REQ-518** — [O] INV-12/INV-19 · [R] spec/012 REQ-1105 (the vote-consuming wait this feeds).
  The system SHALL accept an authenticated operator vote on a POLL_PAUSE-held decision: a
  session-authenticated POST binding {external_ref, action_id, approve} that signals the waiting Runner
  workflow keyed by that external_ref — the vote SHALL name the sealed action it approves (the Runner
  ignores and records a vote naming any other action). The voter identity SHALL be the
  server-authenticated session principal, never a client-supplied claim. The surface SHALL reject a
  cross-origin POST (same-origin enforcement in addition to the SameSite cookie), SHALL rate-limit votes
  per operator, SHALL register only alongside the browser session path, SHALL fail closed to unavailable
  when no workflow signaler is wired, SHALL report a closed decision window (409) ONLY when no session is
  waiting on that ref, and SHALL report a transient delivery failure as retryable (503) — never disguised
  as a closed window. A duplicate or late vote can never double-release an action.

- **REQ-519** — [O] INV-01 (mandatory auth, default-deny) · [O] INV-13 (secrets by reference).
  The system SHALL admit push sources that cannot HMAC-sign a body (e.g. Alertmanager webhooks) to the
  ingest front door via a per-source STATIC bearer token, as an ingest-route-only class beside HMAC and
  mTLS, without weakening either: on an ingest-push route the machine HMAC/mTLS path SHALL be tried
  FIRST and unchanged, and ONLY when no such credential is present SHALL a bearer token be considered.
  The token SHALL be compared in constant time, keyed to the {source_type} URL slug, resolved by secret
  reference (never a literal), and the path SHALL fail closed for a source with no ingest token
  provisioned, an unknown source, or an absent/malformed Authorization header — observationally
  identical to any other unauthenticated request, rejected before the handler reads the body. A bearer
  principal exists ONLY on the ingest-push route class; it can never satisfy an HMAC, mTLS, read-only,
  or session route. A static token carries no replay protection, so an ingest-push route SHALL be
  TLS-fronted in production deployment. WHEN a grouped-transport source (its ingester implements the
  batch extension) posts a webhook carrying multiple alerts, the front door SHALL fan each normalized
  envelope out to its own idempotent triage session (keyed by external_ref, reject-duplicate) and
  report per-incident acceptance; an empty normalization result from a well-formed webhook SHALL be
  accepted with nothing triaged, and a triage-backend failure SHALL be retryable (502), never a silent
  drop.

- **REQ-520** — [R] task #27 Phase A · [O] INV-15 (report the resolver's truth), INV-13 (no secret
  values). The system SHALL serve a read-only control-plane configuration surface: every knob of the
  compiled registry with its resolved value, its source (law / env / console / default), and its
  law / console-writable flags, produced by a layered resolver in which a console override is honored
  ONLY for a console-writable non-LAW key, an env value covers the remaining non-LAW keys, and a LAW
  key ALWAYS resolves to its compiled value — no env or console entry can reach it (the clamp is
  structural). The surface SHALL emit no secret value (secrets keep the value-less reference surface,
  REQ-512) and SHALL fail closed to unavailable when no resolver is wired. GET-only, authenticated
  like every read route.

- **REQ-521** — [R] spec/010 consumer · [O] INV-15 (serve recorded knowledge, never fabricate it).
  The system SHALL serve a read-only **wiki** surface — the living knowledge base — composed of three
  sections: **lessons**, the distilled resolved-incident corpus the worker maintains (read from the
  same corpus file the retrieval plane reloads, each entry keyed by its citable `external_ref`, with
  only recorded fields served — no invented confidence or timestamp); **runbooks**, curated operator
  pages embedded in the binary at build time (the deployed grounder is a static image with no docs
  tree on disk); and **skills**, the production skill library joined by reference from the existing
  skill read surface with its availability honestly flagged. The surface SHALL serve an index and a
  per-page detail resolved by exact slug lookup whose body is the recorded markdown; an unknown slug
  SHALL return not-found; an absent or unconfigured corpus file SHALL yield an empty lessons section
  and a malformed corpus SHALL surface as an error — never an empty fabrication; the lessons list
  SHALL be bounded with the true corpus total reported; and the surface SHALL fail closed to
  unavailable when no wiki reader is wired. GET-only, authenticated like every read route.

- **REQ-522** — [O] INV-01 (default-deny; a new tier weakens no existing lane) · [R] task #27 Phase B
  (the signed-off admin lane). The system SHALL provide an admin operator tier as a distinct
  structural route class (`AuthAdminSession`) for control-plane WRITE surfaces, obtained ONLY by
  step-up re-authentication: WHEN a caller holding a valid operator session presents the SEPARATE
  admin credential (admin name + admin token, both supplied via secret references, only a SHA-256
  digest of the token held in process) to the elevation route, the system SHALL mark that server-side
  session admin-elevated for a bounded short TTL; a route registered `AuthAdminSession` SHALL admit
  only a valid, currently-elevated session and SHALL produce a principal carrying the admin
  capability. Elevation attempts SHALL be rate-limited per (admin, source-ip) and compared in
  constant time; a machine principal (HMAC/mTLS/bearer), a plain session, an expired elevation, and
  an absent or wrong credential SHALL all be rejected as one indistinguishable unauthenticated
  response, and an `AuthAdminSession` route SHALL never inspect HMAC/mTLS material. An unconfigured
  admin authenticator SHALL mean the elevation route and every admin write route are not registered
  at all (fail closed), and the read, vote, and skill surfaces SHALL be unchanged by the tier's
  existence. A session established through a directory-backed login whose user is a member of the
  configured admin group (REQ-508) SHALL be admin-eligible: it MAY step up to the `AuthAdminSession`
  tier on that proven group membership ALONE, without presenting the separate static admin credential,
  and the resulting elevation SHALL be identical in class, capability, and bounded TTL to a
  credential-based step-up; a directory-backed operator (non-admin-group) session and the static
  break-glass operator SHALL NOT be admin-eligible and MAY step up only via the static admin credential.

- **REQ-523** — [O] INV-19 (ledger-before-commit) · [R] task #27 Phase C (config writes GA).
  The system SHALL accept an admin-session write to a control-plane configuration key — a POST
  binding {value, rationale} to a registered key, rationale mandatory at the surface AND re-validated
  in the single writer. The write SHALL be refused with 422 for a LAW key (the resolver clamp is the
  law) and for a non-console-writable key, with 404 for an unregistered key, and with 400 for an
  out-of-bounds value or a missing rationale. An accepted write SHALL execute in the WORKER — the
  governance ledger's single writer — through a distinctly-named Temporal workflow that appends the
  decision to the hash-chained governance ledger BEFORE the `control_plane_config` row commits (a
  crash leaves an over-recorded ledger, never an unrecorded override), and the row SHALL carry the
  server-authenticated operator, the rationale, the ledger sequence, and its stamped
  `schema_version`. The resolver SHALL report the committed override with source=console on the next
  resolve and the composition SHALL adopt legal overrides at boot so the report and the running
  components agree (INV-15); a cross-origin POST SHALL be rejected, writes SHALL be rate-limited per
  operator, and an unwired write backend SHALL fail closed to 503 — never a silent accept.

- **REQ-524** — [O] INV-13 (no secret value in any artifact or response) · [R] task #27 Phase D
  (sealed secrets). The system SHALL accept an admin-session write of secret MATERIAL as a
  WRITE-ONLY sealed secret: the value SHALL be envelope-encrypted in the receiving process — a fresh
  per-secret data key encrypts the value, the data key is wrapped by a master key resolved per
  request from a secret reference (env:/file:) and discarded, both seals AEAD-bound to the secret's
  name — and ONLY the sealed ciphertext SHALL transit the worker workflow, which ledger-records the
  write (name and ciphertext digest, never material) BEFORE the `sealed_secret` row commits. The
  stored secret SHALL be reachable ONLY as a resolvable reference (`store:<name>`) consumed by the
  control plane's secret-reference resolver; no read surface SHALL emit the value — the secrets
  surface lists sealed names with purpose and timestamps through a response type that carries no
  value field — and the write response SHALL carry the reference and the ledger sequence, never the
  value. An unresolvable master key SHALL fail closed: sealing writes and `store:` resolution are
  unavailable rather than degraded to plaintext.

- **REQ-525** — [O] INV-09 (fail closed), INV-19 (ledger-recorded) · [R] Phase-2 canary readiness review
  §4.B (runtime kill-switch + live metrics). The system SHALL provide a runtime, in-process mutation
  kill-switch and a read-only metrics exposition, both SAFETY-ADDITIVE — each can only turn mutation more
  off or read state, never enable it. The mutation gate SHALL expose an idempotent `Disable` that
  atomically turns mutation off without a process restart and SHALL NOT re-enable it (no argument, the
  preflight bit untouched). The worker — the process that owns the gate — SHALL serve a HALT-ONLY admin
  endpoint (`POST /halt`) that requires a bearer token resolved by secret reference, calls `Disable`, and
  records the halt to the hash-chained governance ledger bound to a synthetic action_id; the halt endpoint
  SHALL NOT be registered when the token reference does not resolve (fail closed), and that admin surface
  SHALL carry no enable route. An armed mutation breaker SHALL, on a post-execution deviation verdict or a
  chain-integrity gap reaching a configurable threshold (default 1), call `Disable` automatically; under
  mutation off nothing executes, so it SHALL stay inert. Both the grounder and the worker SHALL serve a
  read-only Prometheus text `/metrics` exposition reporting `mutation_enabled` (0 while the read-only
  foundation holds) and, where a breaker is wired, `circuit_breaker_state` and `deviation_count`, emitting
  no secret value; the exposition SHALL be scraped by a deployed Prometheus whose alert rules fire on an
  unexpected `mutation_enabled == 1`, an open mutation breaker, and an observed deviation.

- **REQ-526** — [R] spec/016 credential-engine consumer (TG-107/TG-89) · [O] INV-13 (no secret value in any
  response), INV-15 (serve real persisted state, never a fabricated row). The system SHALL serve a read-only
  **credentials** surface over the credential engine's REAL persisted projections — the worker-written
  per-source sync/drift state (`credential_sync_run` + `credential_coverage`, REQ-505-stamped-exempt
  operational projection) and the append-only per-target resolution audit (`credential_resolution`,
  REQ-1617). It SHALL expose three GET routes, each read-only and authenticated (INV-01): **sources** — the
  latest sync run per source with its plane, last-synced time, drift (added/changed/removed), current
  covered-target count, and outcome, ordered plane then source_id; **resolutions** — the recent resolution
  history newest-first (bounded by a server cap, optionally filtered to one `?target=`, `?limit=` clamped)
  as the target/plane/outcome/source/native/rule/user/scheme/key-ref-scheme/shadowed/error metadata; and
  **coverage** — a summary DERIVED from the recent resolution window: resolved/unresolved/ambiguous tallies
  per plane and per source plus each target's most-recent resolved-vs-refused outcome (the coverage
  frontier). Every response SHALL carry ONLY non-secret identity metadata — a login user, a connection
  scheme, and the SCHEME of a key reference (env/file/store/vault/bao) — and SHALL NEVER carry key material,
  a `SecretRef` value or path, or a token: the read store selects only the non-secret columns the projection
  tables hold, which are secret-free by construction. An empty projection SHALL return an honest empty result
  and an unwired store SHALL fail closed to unavailable — never a fabricated source, resolution, or coverage
  figure. The surface SHALL NOT perform a live resolve-probe ("can TG reach host X now?"); coverage is
  derived from persisted history — a live probe needs the worker-side resolver and is a documented follow-up.

## Persistence contract

Three governed tables are owned by this spec's persistence surface. `session_risk_audit` (the required
per-classification decision record, INV-19) is appended to the hash-chained governance ledger.
`discovered_scheduled_reboots` is the bi-temporal schedule registry (`valid_from`,
`valid_until`, `kill_switch`, `observing`/`live`). `escalation_queue` is the append-only rate-capped
requeue lane (`attempts`, `status`, `eligible_at`). Every row of all three carries
`schema_version` and is read under authority-checked RBAC. See [`docs/DATA-MODEL.md`](../../docs/DATA-MODEL.md).

## Generated-contract invariant

A standing CI check SHALL FAIL if any routed endpoint is absent from the generated `openapi.yaml` /
`asyncapi.yaml`, if a count in the generated contract was hand-written, if the generated artifact drifts
from the canonical Go/Postgres model, or if the `generated_at` / source-hash / coverage-scope provenance
is missing (REQ-501b, INV-15).
