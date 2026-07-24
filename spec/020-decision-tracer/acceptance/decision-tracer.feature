# spec/020 — Governed decision tracer acceptance oracles.
# Nothing is built yet: this is the SDD DESIGN GATE. Every scenario specifies behavior of the not-yet-built
# decision-signal persistence (Tier-0), the per-step evidence tables (agent_step, interceptor_gate_verdict),
# the per-session detail read endpoint, and the console live-wiring — and is tagged @pending, tracked as
# declared debt in acceptance/_test_mapping.json and skipped by the runner (Tags: "~@pending") until the
# owning task lands. The tracer is READ-ONLY / OBSERVE-ONLY: it sits OFF every actuation chokepoint, it
# authorizes nothing, and it changes no mutation posture; every credential field it persists or returns is a
# reference or scheme, never a secret value (INV-13), and every agent thought/tool-result is Scrubbed before
# it reaches any trace table (INV-08).
Feature: The decision tracer stitches one incident into an ordered, non-secret, read-only per-step walk

  For a single incident the tracer joins ingest -> classify -> agent ReAct cycles -> parse -> each interceptor
  gate -> policy Decide -> credential Resolve -> regime select -> execute -> verify into one ordered walk, and
  for each step shows the rule, matched rules, rationale, tools, prompts, skills, confidence against its
  min_confidence threshold, band, mechanical verdict, each gate's verdict-and-reason, the in-force ACL bundle
  version, and the credential identity as references and schemes only. It persists the decision signal that is
  computed in memory today and discarded at the database boundary, adds the authenticated detail read endpoint
  that assembles it, and live-wires the existing console Workflows view — without changing the decision path.

  @REQ-2000
  Scenario: The trace record carries every decision-grade field for a step
    Given a sealed session with a classified action that traversed the interceptor chain
    When the tracer assembles the per-step trace record
    Then each step carries the rule matched_rules reason tools prompts skills confidence min_confidence band verdict gate bundle_version and credential fields and the credential fields are references and schemes only

  @REQ-2001 @pending
  Scenario: Every decision chokepoint emits an observe-only trace step at its boundary
    Given the decision path runs classify each interceptor gate each ReAct cycle policy Decide credential Resolve regime select and verify
    When the trace sink is present and when it is absent
    Then each boundary emits one nil-safe side-write when the sink is present and the emitter degrades to a no-op when the sink is absent and the boundary is unaffected either way

  @REQ-2002
  Scenario: The tracer sits off every chokepoint and changes no mutation posture
    Given the same decision replayed with tracing on and with tracing off
    When the never-auto floor the policy verdict the credential resolution and the mode chokepoint run
    Then every gate behaves identically whether or not the decision is traced and no tracer emit or read path reaches an actuator or returns a value that gates actuation

  @REQ-2003 @pending
  Scenario: The confidence scalar persists and reads back non-zero
    Given a proposal whose model confidence is parsed as a non-zero scalar
    When the action is sealed and the decision record is read back from the real database
    Then the confidence scalar is threaded into the execute request and persisted and reads back non-zero rather than clamped to zero at the database boundary

  @REQ-2004
  Scenario: A policy_decision row carries the in-force bundle_version and the matched-rules list
    Given a policy evaluation that computes a bundle_version a matched-rules list and a reason in memory
    When the policy-decision row is written and read back
    Then the row carries a bundle_version equal to the in-memory value and a matched_rules list whose length equals the in-memory matched-rules length and the decision reason

  @REQ-2005
  Scenario: The policy Decide audit carries the action_id external_ref and principal keys
    Given a policy Decide evaluation on a classified action
    When the audited engine writes its per-evaluation row
    Then the row carries the action_id the external_ref and the principal so the policy record joins into the walk by both correlation keys rather than empty key columns

  @REQ-2006
  Scenario: The action_manifest backfills the approval choice and the post-execution verdict
    Given an action that was approved and then executed and verified
    When the manifest-lifecycle writer seals the row
    Then the action_manifest carries the approval_choice and the post-execution verdict rather than leaving them NULL

  @REQ-2007
  Scenario: All gates of a passing action leave ordered verdict rows
    Given an action that passes every interceptor gate
    When the interceptor chain runs
    Then the tracer persists one ordered interceptor_gate_verdict row per gate keyed by both action_id and external_ref in gate order

  @REQ-2007
  Scenario: A refusing action leaves the refusing gate row and no phantom pass rows past it
    Given an action that is refused at one interceptor gate
    When the interceptor chain runs and returns at the refusal
    Then the tracer persists the refusing gate's row and no phantom pass rows for gates past the refusal

  @REQ-2008
  Scenario: Each ReAct cycle leaves a scrubbed agent-step record with no secret value
    Given an agent loop whose thoughts observations and tool-results may contain secret-shaped text
    When each ReAct cycle is persisted
    Then one agent_step row per cycle is written keyed by external_ref every field is run through the Scrub redaction path before write and the row carries no secret value and the thoughts never re-enter the decision path as control flow

  @REQ-2009
  Scenario: The session record carries the seed hash prompt version and model tier
    Given a classified session composed from a seed a prompt version and a model tier
    When the session triage record is written
    Then the record carries the seed_hash the prompt_version and the model_tier so the inspector shows which prompts and tier composed each step

  @REQ-2010
  Scenario: The tracer covers queued live-running and executed sessions
    Given a queued session a live-running session and an executed session
    When the operator inspects each
    Then the executed session is served by the detail read endpoint and the queued and running sessions animate from real boundary events over the step channel so no session state is invisible

  @REQ-2011
  Scenario: The detail endpoint returns the ordered steps of one session over an authenticated read
    Given a sealed session and the detail read endpoint GET /v1/sessions/{external_ref}
    When an authenticated principal requests the session detail
    Then the endpoint requires authentication and returns the per-step trace record in decision-boundary order assembled from the correlation spine and the per-step tables

  @REQ-2012 @pending
  Scenario: The console workflows view renders real trace steps in place of the fixtures
    Given the console Workflows view and the detail read endpoint
    When the operator opens a session and clicks each step
    Then the view renders real trace steps from the endpoint in place of the WF_RUNS fixtures the fixture is retained as the offline render contract and the assemble.py build byte-reproduces the served index.html

  @REQ-2013
  Scenario: A queued or running session animates from real boundary events over the step channel
    Given a queued or running session and the per-session step channel
    When the session crosses each decision boundary
    Then the console animates the session from real boundary step events over an authenticated channel rather than a client-side simulation clock

  @REQ-2014
  Scenario: The trace surface is gated behind a distinct trace-read role
    Given the detail endpoint the step channel and the console walk and a principal without the trace-read role
    When the principal requests the trace surface
    Then the trace surface is gated behind a distinct elevated trace-read role separate from the current read-only surface and the principal without it is refused

  @REQ-2015
  Scenario: The detail endpoint projects references and schemes only and never a secret value
    Given a session whose steps reference credentials and secret-bearing fields
    When the detail endpoint projects the session
    Then every credential and secret-bearing field is a SecretRef reference or a scheme and no field matches a value-shaped secret denylist

  @REQ-2016
  Scenario: The runtime role holds no update or delete on the agent-step and gate-verdict evidence tables
    Given the append-only agent_step and interceptor_gate_verdict tables
    When the runtime database role attempts to update or delete a persisted trace row
    Then the update or delete is denied by the grants because the runtime role holds no UPDATE and no DELETE on these evidence tables

  @REQ-2017 @pending
  Scenario: The trace schema separates the estate-specific layer from the generalizable layer
    Given the trace schema
    When the estate-specific layer and the generalizable layer are inspected
    Then the estate-specific layer holds hosts IPs topology credential identities and raw traces and the generalizable layer holds alert-class to resolution to verified-outcome plus graduated artifacts with no estate identifier so a future federated export needs no schema rewrite and v1 shares nothing

  @REQ-2018
  Scenario: A past decision joins the exact immutable ruleset version in force when it decided
    Given a policy_decision written under one ruleset and a later ruleset change
    When the past decision is joined to the versioned ruleset record by its bundle_version
    Then it resolves the exact immutable ruleset document in force when it decided rather than the singleton latest-wins state

  @REQ-2019
  Scenario: The ingest to verify walk joins end to end with no bridge hop
    Given a session that did and a session that did not seal an action with external_ref reached onto the actuation-side rows
    When the ingest to verify walk is assembled
    Then both walks join end to end by external_ref with no bridge hop and the walk remains complete for the session that sealed no action

  @REQ-2020 @pending
  Scenario: The optional export lane carries the redacted LLM subset only and is off by default
    Given the optional export lane over an OTel span backbone
    When the lane is disabled and when it is enabled
    Then nothing is exported while the lane is disabled and when enabled it carries the redacted LLM subset only never the governance fields never becomes the system of record and never sits on the decision path

  @REQ-2021
  Scenario: The calibrator bins stated confidence against verified outcomes into a reliability curve
    Given persisted agent confidence on session_triage and the LLM-free mechanical verified outcomes joined by external_ref
    When the read-only calibrator scores the paired confidence and outcomes
    Then it emits a reliability curve binning stated confidence against the verified-clean rate plus a Brier or ECE score and it adjudicates nothing and gates nothing and leaves the decision path unchanged
