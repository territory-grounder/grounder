# spec/006 — Interface contracts acceptance oracles.
# core/auth already implements the mandatory-auth path (REQ-501). Per the migration note every
# scenario here is tagged @pending: the parent binds the green ones against core/auth and the rest
# against core/httpapi / core/ingest / core/audit / core/schema / core/persist as they land. The
# runner skips @pending (Tags: "~@pending"); their honest status is declared in _test_mapping.json.
Feature: Interface contracts gate every boundary through one authenticated, generated surface

  The outside estate reaches the governed spine only through the mandatory auth middleware, the
  canonical triage.requested event, the version-stamped persistence contracts, and the generated
  wire contracts. Each is a mechanical property that no caller input can bypass.

  @REQ-501
  Scenario: An unauthenticated stats request is rejected before body parse
    Given a stats route registered on the authenticated router
    When a request arrives with no valid credential
    Then the response is 401 and the request body is never parsed

  @REQ-501
  Scenario: A route registered with auth=none fails to register at boot
    Given a route declared with auth method none
    When the router registers the route
    Then registration panics at boot and no open endpoint is created

  @REQ-501
  Scenario: A replayed nonce is rejected
    Given a valid HMAC request whose nonce was already seen for its source
    When the router authenticates the request
    Then the request is rejected as a nonce replay

  @REQ-501
  Scenario: A stale-timestamp request is rejected
    Given an HMAC request whose timestamp is outside the replay window
    When the router authenticates the request
    Then the request is rejected as a stale timestamp

  @REQ-501
  Scenario: A tampered body invalidates the HMAC signature
    Given an HMAC request whose body was altered after signing
    When the router authenticates the request
    Then the signature does not verify and the request is rejected

  @REQ-501
  Scenario: Session-replay mints a new gated workflow from a read-only snapshot
    Given an authenticated session-replay request for a known session
    When the replay handler processes the request
    Then a new Temporal workflow is started from an immutable read-only ContextSnapshot
    And no mutating session is resumed with caller-supplied input

  @REQ-504
  Scenario: A replay lookup for an unknown id returns not-found
    Given an authenticated replay request for an id that does not exist
    When the replay handler resolves the id under the caller's authority
    Then the response is not-found

  @REQ-504
  Scenario: An id the caller has no authority over is indistinguishable from a missing id
    Given an authenticated replay request for an id owned by a different role
    When the replay handler resolves the id under the caller's authority
    Then the response is not-found and reveals nothing about the foreign row

  @REQ-501b
  Scenario: Generated wire contracts cover every routed endpoint with provenance
    Given the router's registered routes and the canonical Postgres entities
    When the contract generator emits openapi.yaml, asyncapi.yaml, and the JSON Schemas
    Then every routed endpoint is present with a declared auth and error schema
    And the artifact carries a non-null generated_at, source hash, and coverage scope

  @REQ-502
  Scenario: An ingest adapter publishes a triage.requested event keyed by external_ref
    Given an ingest adapter that received a provider alert
    When the adapter finishes normalization
    Then a triage.requested event is published to the routing layer keyed by its external_ref

  @REQ-502
  Scenario: Ingest runs dedup, flap, burst, and correlate before publishing
    Given a burst of duplicate and flapping provider alerts
    When the ingest adapter processes them
    Then dedup, flap, burst, and correlate run in code before any triage.requested is published

  @REQ-503
  Scenario: Every risk classification appends one session_risk_audit row to the ledger
    Given a completed risk classification
    When the decision is recorded
    Then exactly one required-field session_risk_audit row is appended to the hash-chained ledger

  @REQ-505
  Scenario: Every governed row is stamped with its writer's schema_version
    Given a writer inserting a row into a governed table
    When the row is persisted
    Then the row carries the current schema_version from the canonical registry

  @REQ-505
  Scenario: A reader rejects a row written by a future schema version
    Given a governed row whose stored schema_version exceeds the reader's compiled version
    When the reader decodes the row
    Then the reader returns a SchemaVersionError instead of mis-reading the row

  @REQ-506
  Scenario: The discovered_scheduled_reboots registry is bi-temporal with a kill switch
    Given a discovered scheduled-reboot schedule
    When the schedule is registered
    Then the row carries valid_from, valid_until, an observing or live state, and a kill_switch

  @REQ-507
  Scenario: The escalation_queue is append-only and re-enters the gated pipeline
    Given an unanswered approval poll whose session archived
    When a delayed re-check row is enqueued and later fires
    Then the row is append-only and re-enters the gated pipeline as an authenticated Temporal signal

  @REQ-501
  Scenario: Registering a route with auth set to none panics at registration
    Given an auth router
    When a route is registered with the auth-none method
    Then route registration panics

  @REQ-501
  Scenario: A verifier without replay protection fails closed at construction
    When a verifier is constructed with no nonce store
    Then construction returns the replay-protection-unconfigured error

  @REQ-508
  Scenario: An operator login issues a read-only session that satisfies the read surface
    Given a session-enabled interface surface with operator "kyriakos"
    When the operator logs in with the valid token
    Then a session cookie is issued
    And a session GET of the stats route succeeds as "operator:kyriakos"

  @REQ-508
  Scenario: A session principal never satisfies a machine route and never writes
    Given a session-enabled interface surface with operator "kyriakos"
    And the operator logs in with the valid token
    When the session cookie is presented to the ingest route
    Then the request is rejected unauthenticated before the handler runs
    When the session performs a POST against the stats route
    Then the request is rejected as read-only

  @REQ-508
  Scenario: Tampered, expired, and revoked session cookies are rejected before the handler runs
    Given a session-enabled interface surface with operator "kyriakos"
    And the operator logs in with the valid token
    When the session cookie is tampered with
    Then a session GET of the stats route is rejected unauthenticated
    When the operator logs out
    Then a session GET of the stats route is rejected unauthenticated

  @REQ-508
  Scenario: Operator login is rate-limited per operator and source ip
    Given a session-enabled interface surface with operator "kyriakos"
    When five logins fail with a wrong token
    Then a sixth login is rate-limited even with the valid token

  @REQ-509
  Scenario: The sessions surface serves the audit spine over an operator session
    Given a session-enabled interface surface with operator "kyriakos"
    And the audit spine holds a classified session with a deviation verdict
    And the operator logs in with the valid token
    When the operator lists the recent sessions
    Then the session list carries the spine's band, action id, and verdict unchanged

  @REQ-509
  Scenario: An unwired sessions surface fails closed instead of fabricating rows
    Given a session-enabled interface surface with operator "kyriakos"
    And the operator logs in with the valid token
    When the operator lists the recent sessions without a wired spine
    Then the sessions request is rejected as unavailable

  @REQ-510
  Scenario: An accepted ingest appears on the alerts surface with its validated fields
    Given a session-enabled interface surface with an alert log and operator "kyriakos"
    And the operator logs in with the valid token
    When an authenticated source ingests a valid alert payload
    And the operator lists the recent alerts
    Then the alert list carries the accepted envelope's rule and severity unchanged

  @REQ-510
  Scenario: A rejected payload never reaches the alerts surface
    Given a session-enabled interface surface with an alert log and operator "kyriakos"
    And the operator logs in with the valid token
    When an authenticated source ingests a grammar-violating payload
    And the operator lists the recent alerts
    Then the alert list is empty

  @REQ-511
  Scenario: The governance surface reports the posture its components hold
    Given a session-enabled interface surface with governance state and operator "kyriakos"
    And the operator logs in with the valid token
    When the operator reads the governance surface
    Then the posture carries mutation off, the spine's band counts, and the chain head unchanged

  @REQ-512
  Scenario: The secrets surface carries references and resolution only, never a value
    Given a session-enabled interface surface with governance state and operator "kyriakos"
    And the operator logs in with the valid token
    When the operator reads the secrets surface
    Then the reference list carries the reference and resolution state
    And the response never contains the resolvable secret value

  @REQ-513
  Scenario: The events stream emits the live posture on connect
    Given a session-enabled interface surface with governance state and operator "kyriakos"
    And the operator logs in with the valid token
    When the operator connects to the events stream
    Then the first event is a posture snapshot carrying mutation off and the chain head

  @REQ-514
  Scenario: The models surface relays the gateway report verbatim and fails closed when unwired
    Given a session-enabled interface surface with a model gateway report and operator "kyriakos"
    And the operator logs in with the valid token
    When the operator reads the models surface
    Then the gateway report is relayed verbatim
    When the operator reads the models surface without a wired gateway
    Then the models request is rejected as unavailable

  @REQ-515
  Scenario: The contract surface serves the generated endpoint map verbatim
    Given a session-enabled interface surface with the embedded contract and operator "kyriakos"
    And the operator logs in with the valid token
    When the operator reads the contract surface
    Then the generated OpenAPI document is served verbatim

  @REQ-516
  Scenario: The estate surface serves the latest published graph snapshot
    Given a session-enabled interface surface with a published estate snapshot and operator "kyriakos"
    And the operator logs in with the valid token
    When the operator reads the estate surface
    Then the estate carries the published nodes and confidence-weighted edges unchanged

  @REQ-516
  Scenario: The estate surface reports no snapshot rather than fabricating a topology
    Given a session-enabled interface surface with no estate snapshot and operator "kyriakos"
    And the operator logs in with the valid token
    When the operator reads the estate surface
    Then the estate reports it is unavailable with no nodes

  @REQ-517
  Scenario: The grounding scorecard publishes the verifier distribution and the falsifiability signal
    Given a session-enabled interface surface with a scored grounding spine and operator "kyriakos"
    And the operator logs in with the valid token
    When the operator reads the grounding scorecard
    Then the scorecard carries the verdict distribution and the falsifiability signal

  @REQ-517
  Scenario: The grounding scorecard reports honest zeros over an empty spine
    Given a session-enabled interface surface with an empty grounding spine and operator "kyriakos"
    And the operator logs in with the valid token
    When the operator reads the grounding scorecard
    Then the scorecard reports honest zeros rather than a fabricated rate

  @REQ-518
  Scenario: An authenticated operator vote signals the waiting decision it answers
    Given a session-enabled interface surface with a waiting decision and operator "kyriakos"
    And the operator logs in with the valid token
    When the operator votes to approve the waiting decision
    Then the vote is delivered bound to that decision with the session identity as voter

  @REQ-518
  Scenario: A vote on a closed decision window reports conflict, never success
    Given a session-enabled interface surface with a waiting decision and operator "kyriakos"
    And the operator logs in with the valid token
    When the operator votes on a decision no session is waiting for
    Then the vote is rejected as a closed decision window

  @REQ-520
  Scenario: The config surface reports each knob with its source and the LAW clamp holds at resolve
    Given a control-plane config resolver with law, env, and console layers
    When the configuration is resolved
    Then a LAW key resolves to its compiled value whatever env or console hold
    And a console override is honored only for a console-writable non-LAW key

  @REQ-522
  Scenario: Step-up elevation grants the admin write tier on the same session
    Given a session-enabled interface surface with an admin credential configured
    And a logged-in operator session that is not elevated
    When the operator presents the admin credential to the elevation route
    Then the same session satisfies an admin-session write route with the admin capability

  @REQ-522
  Scenario: The admin tier fails closed when unconfigured and refuses non-elevated callers uniformly
    Given a session-enabled interface surface without an admin authenticator
    When any caller posts to the elevation route or an admin write route
    Then the routes are not registered and every caller is refused

  @REQ-523
  Scenario: A config write to a LAW key is refused as unprocessable — the clamp is the law
    Given an admin-elevated operator session
    When the operator posts an override for a LAW key
    Then the write is refused with 422 and never reaches the write backend

  @REQ-523
  Scenario: An accepted config write is ledgered before the override row commits
    Given the worker's config-write activity with a governance ledger and an override store
    When a legal override is applied
    Then the ledger holds the decision and the row carries its sequence
    And a store failure after the append leaves an over-recorded ledger, never an unrecorded change

  @REQ-524
  Scenario: A sealed secret round-trips through envelope encryption and refuses a wrong key or name
    Given a master key generated for the test
    When a value is sealed under a name and opened again
    Then the value round-trips, and a wrong key, wrong name, or tampered blob refuses to open

  @REQ-524
  Scenario: The secret write surface is write-only and the read surface lists names without values
    Given an admin-elevated operator session and a sealed-secret backend
    When the operator seals a secret value
    Then the response carries the store reference and never echoes the value
    And the secrets read surface lists the sealed name with no value field

  @REQ-525
  Scenario: The mutation gate exposes a runtime disable that keeps mutation off
    Given a mutation gate on the read-only foundation
    When the runtime disable is invoked twice
    Then mutation stays off and the gate refuses every mutation

  @REQ-525
  Scenario: The safety metrics exposition renders mutation_enabled zero and the breaker gauge
    Given the read-only safety gate and an armed mutation breaker
    When the metrics exposition is rendered
    Then it reports mutation_enabled 0 and the circuit breaker gauge, and no secret
