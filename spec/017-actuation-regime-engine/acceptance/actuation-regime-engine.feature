# spec/017 — Actuation Regime Engine acceptance oracles.
# Nothing is built yet: this is the SDD DESIGN GATE (TG-110). Every scenario specifies behavior of the
# not-yet-built core/regime.Engine and the modules/actuation/awxjob lane (Phase 2; the read-only sensor +
# knowledge lanes are Phase-1-safe) and is tagged @pending, tracked as declared debt in
# acceptance/_test_mapping.json and skipped by the runner (Tags: "~@pending") until the owning task lands.
# Every lane is an effect leaf that plugs INTO the spec/013 interceptor beneath the mode chokepoint; it
# never lifts the constitutional mechanical never-auto floor (INV-09) and never bypasses policy/credential.
Feature: The actuation regime engine applies a change through the regime-aware effect lane its target sanctions

  Regime resolution is config-not-code and fails closed (unknown/ambiguous regime → default lane or refuse);
  every lane is reachable only through the spec/013 interceptor chain; the AWX-job lane (the first non-SSH
  lane) launches only an allowlisted, policy-authorized job template with typed extra_vars and a
  credential-resolved token, and only while the mode chokepoint permits; an async job is a prediction
  verified by a deferred-verify channel and fed to the graduation ladder; a companion knowledge lane ingests
  sanctioned playbooks read-only so the agent proposes them.

  @REQ-1700
  Scenario: A target resolves to exactly one regime and its lane
    Given operator-declared regime rules mapping host glob group and device-class to one of the five regimes
    When the engine selects a lane for a target
    Then exactly one regime is resolved and its bound effect lane is returned from the config data with no code change

  @REQ-1701
  Scenario: An unknown regime falls back to the default lane or refuses and an ambiguous regime fails closed
    Given a target that matches no regime rule and a separate target that matches more than one regime
    When the engine selects a lane for each
    Then the unknown target resolves to the operator default native-ssh lane or refuses and the multi-regime target fails closed and refuses rather than choosing a lane arbitrarily

  @REQ-1702
  Scenario: Every lane is reachable only through the actuation interceptor chain
    Given a selected effect lane
    When code attempts to reach the lane's effect
    Then the only path to the effect is the spec/013 interceptor Do and there is no exported path that bypasses the never-auto floor the policy verdict the credential resolution or the mode chokepoint

  @REQ-1703
  Scenario: Regime resolution matches on the same estate object-model as policy and credential
    Given one estate object-model of host glob group device-class and inventory
    When the regime resolver and the policy and credential engines match a target
    Then all three key off the same object-model and no second inventory grammar is defined

  @REQ-1704
  Scenario: The AWX-job actuator launches only an allowlisted policy-authorized template
    Given an operator template-allowlist binding job templates to op-classes and a policy engine verdict
    When the AWX-job actuator is asked to launch a template
    Then it launches only an allowlisted template whose op-class the policy engine did not deny and refuses a non-allowlisted or policy-denied template

  @REQ-1705
  Scenario: The AWX-job effect is a template plus typed extra_vars and rejects unknown keys
    Given a job template with an operator-declared extra_vars schema
    When the actuator builds the launch body
    Then the effect is the template id plus typed extra_vars validated against the schema an unknown extra_vars key is rejected and no free-form command string is passed

  @REQ-1706
  Scenario: The AWX-job lane resolves the token after policy and launches only while the mode chokepoint permits
    Given a classified AWX-job action the policy engine authorized with a non-deny verdict
    When the actuation chokepoint reaches the target
    Then the AWX API token is resolved through the credential engine after authorization and before launch and the job launches only while the mode chokepoint permits actuation

  @REQ-1707
  Scenario: A read-only setup job is a sensor and a mutating template stays off until the flip
    Given a read-only AWX setup fact-gathering job and a mutating job template
    When the engine runs each
    Then the setup job is a Phase-1-safe sensor and the mutating template routes through the mode chokepoint and the never-auto floor and stays off while the mode is Shadow until the owner-present flip

  @REQ-1708
  Scenario: The AWX token is a sealed SecretRef and the sensor token is read-only and distinct
    Given the AWX-job lane and the read-only sensor and knowledge lane
    When each authenticates to AWX
    Then every token is a sealed SecretRef with no plaintext in config the ledger or any exportable artifact and the sensor token is a read-only token declared distinctly from any launch-capable token

  @REQ-1709
  Scenario: An async actuation returns a job handle and is polled to a terminal state
    Given an AWX job launched asynchronously
    When the deferred-verify channel runs
    Then the lane returns a job handle and the engine polls the job to a terminal state rather than declaring success at launch

  @REQ-1710
  Scenario: The launch is a prediction verified against the terminal outcome and fed to the graduation ladder
    Given a launched job treated as a prediction
    When the job reaches a terminal outcome
    Then the engine computes the mechanical verdict by comparing the terminal outcome against the prediction and feeds the verdict to the graduation ladder

  @REQ-1711
  Scenario: A launched but unverified job counts as no clean run and times out to unverified
    Given a launched job that has not reached a terminal deferred-verified outcome
    When graduation reads the run and the verification bound elapses
    Then the job is recorded pending-verification and counts as no clean verified run and a job that does not reach terminal within the bound is recorded unverified and counts toward no graduation

  @REQ-1712
  Scenario: A second launch for the same action_id is refused
    Given an action_id that already carries a live or terminal job
    When a retry re-poll or redelivery attempts a second launch for that action_id
    Then the engine refuses the second launch so the action never double-actuates

  @REQ-1713
  Scenario: The knowledge lane ingests templates and inventory read-only re-read by id
    Given AWX job templates descriptions and inventory
    When the knowledge lane ingests them
    Then it pulls them read-only into the wiki and the RAG plane and re-reads each object from the AWX API by id rather than trusting a cached copy

  @REQ-1714
  Scenario: The knowledge lane launches nothing and only proposes a runbook
    Given a sanctioned runbook the knowledge lane surfaced
    When the agent acts on the discovered runbook
    Then the knowledge lane launches no job and mutates nothing and the runbook enters the pipeline only as a proposal subject to the full interceptor chain

  @REQ-1715
  Scenario: Every resolution launch and verdict is appended to the ledger with no secret and the tables are append-only
    Given the engine resolves a lane launches a job and completes a deferred verify
    When the runtime database role attempts to update or delete a regime_resolution regime_actuation or deferred_verdict row
    Then one row per resolution launch and verdict carrying only non-secret metadata is appended to the tamper-evident ledger and the update or delete is denied by the grants

  # T-017-7 read-only slice (present): the console renders REAL engine state from the GET /v1/regime read API.
  # The step drives the real httpapi regime read DTOs (contract-level) and asserts the exact non-secret wire
  # shape the console panel binds to (frontend/src/panels/regime + api/generated-client.ts).
  @REQ-1716
  Scenario: The console renders the regime map allowlist pending-verification queue and lane coverage
    Given the console regime surface reads the regime read API
    When the operator views the regime state
    Then the read view carries the per-target regime and lane resolutions the actuation tail with each template's authorized op-class the deferred verdicts and the per-lane coverage roll-up as real non-secret engine state

  # T-017-7 write follow-on (pending): the ledger-audited allowlist EDITOR needs an authenticated write API +
  # AuthAdminSession + audit — a bigger, mutation-adjacent build scoped OUT of the read-only MR. Stays @pending
  # (declared debt) until that write path lands.
  @REQ-1716 @pending
  Scenario: Every allowlist edit is appended to the tamper-evident ledger
    Given the console regime allowlist editor and an authenticated admin session
    When the operator edits the AWX template-allowlist
    Then every allowlist edit is appended to the tamper-evident governance ledger through the audited write path

  @REQ-1717
  Scenario: The native-ssh lane can resolve its effect leaf per action target
    Given a per-target native-ssh lane that binds each action's leaf to its own target host
    When the seam applies a governed actuation under an actuating mode and under Shadow
    Then the leaf executes once on the action's target under actuating mode and is refused before execute under Shadow
