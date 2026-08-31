Feature: Credential delivery & secret substrate (spec/022)
  The worker holds no reusable high-value credential at rest: secrets are resolved from OpenBao at use-time,
  the master key lives in an external key service, the actuation identity is just-in-time, and read and
  mutate blast-radii are disjoint. Every scenario is @pending — this is the design gate; scenarios bind to
  real code as tasks T-022-* land.

  @REQ-2200 @pending
  Scenario: High-value secrets are delivered as SecretRefs, never as plaintext environment values
    Given a worker process configured for the estate
    When its process environment is inspected
    Then no high-value credential value appears in the environment
    And each high-value credential is present only as a bao: or file: SecretRef

  @REQ-2204 @pending
  Scenario: An unresolvable credential refuses the operation with no plaintext fallback
    Given the secret substrate cannot resolve a required credential
    When the dependent operation is attempted
    Then the operation refuses
    And no plaintext, default, cached, or last-used credential value is used

  @REQ-2201 @pending
  Scenario: The master key never resides in the worker and seal/unseal calls the external key service
    Given the master key is provisioned in the external key service
    When the worker seals and unseals a secret
    Then the wrap and unwrap are performed by the external key service
    And the worker process never holds the master key material

  @REQ-2202 @pending
  Scenario: The actuation SSH identity is resolved just-in-time and not persisted between actuations
    Given an approved actuation is about to execute
    When the actuator dials the target
    Then the SSH identity is resolved for that single connection
    And no long-lived actuation key persists on the worker between actuations

  @REQ-2207 @pending
  Scenario: A leased actuation credential expires after its bounded lifetime
    Given the substrate issues a leased actuation credential with a bounded lifetime
    When the lifetime elapses
    Then the credential no longer authenticates to the target

  @REQ-2203 @pending
  Scenario: The read-only triage plane cannot resolve an actuation credential
    Given a triage-plane role and an actuation-plane role
    When the triage plane requests the actuation SSH identity or a mutation write-token
    Then the substrate denies the request

  @REQ-2205 @pending
  Scenario: A resolved secret value never reaches a log, export, or error output
    Given a substrate resolution succeeds or fails
    When logs, exports, and error outputs are examined
    Then no resolved secret value appears in any of them

  @REQ-2206 @pending
  Scenario: The image-signing key is resolved from the substrate, not a plaintext CI variable
    Given the container-image signing key stored in the secret substrate
    When an image is signed at build and verified before deploy
    Then the signing key is resolved via a SecretRef from the substrate
    And the signing key does not appear as a plaintext CI variable or repository file
