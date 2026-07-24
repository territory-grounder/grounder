Feature: Secret plane — force a real backend, eliminate secret-zero (spec/024)
  A fresh install can refuse plaintext secrets and require a real backend (OpenBao/Vault primary, Vaultwarden
  and Passbolt second-tier), with the substrate's own bootstrap credential removed from disk. Every scenario
  is @pending — the design gate; scenarios bind to real code as tasks T-024-* land.

  @REQ-2400 @pending
  Scenario: Enforce policy refuses to boot on a plaintext business secret
    Given the secret policy is enforce
    And a business secret reference resolves through the env scheme
    And that reference is not in the permanent exemption set
    When the boot preflight runs
    Then the process refuses to start with a fail-closed error naming the reference
    And no secret value appears in the error

  @REQ-2400 @pending
  Scenario: Off policy preserves pre-feature behavior
    Given the secret policy is off
    And a business secret reference resolves through the env scheme
    When the boot preflight runs
    Then the process starts exactly as before this feature

  @REQ-2400 @pending
  Scenario: Warn policy logs each plaintext reference and continues
    Given the secret policy is warn
    And a business secret reference resolves through the env scheme
    When the boot preflight runs
    Then each plaintext reference is logged
    And the process continues to start

  @REQ-2401 @pending
  Scenario: The permanent exemption set alone may remain plaintext under enforce
    Given the secret policy is enforce
    And only the substrate bootstrap credential and the database connection strings resolve through env or file
    When the boot preflight runs
    Then the process starts
    And the exemption set is not extendable by ordinary configuration

  @REQ-2402 @pending
  Scenario: Every process secret reference is enumerated for the policy
    Given the set of secret references the policy inspects
    When it is compared to the declared secret-reference fields of the worker and grounder
    Then every declared reference is present in the inspected set

  @REQ-2403 @pending
  Scenario: A business secret migrates from env to a backend with an unchanged resolved value
    Given a business secret resolvable through the env scheme
    When its reference is flipped to a backend scheme
    Then the resolved value is byte-identical to the pre-migration value

  @REQ-2404 @pending
  Scenario: A new backend registers read-only at the scheme registry and fails closed
    Given a new secret backend resolver registered under its scheme
    When a reference for an absent secret is resolved
    Then the resolution fails closed
    And the backend exposes no write path

  @REQ-2405 @pending
  Scenario: The Vaultwarden resolver decrypts a vault item field in native Go
    Given a Vaultwarden reference naming a collection item and field
    And an account credential resolved from a secret reference
    When the reference is resolved
    Then the decrypted field value is returned
    And no Bitwarden Secrets Manager access token is required

  @REQ-2406 @pending
  Scenario: The Passbolt resolver retrieves a resource field via an OpenPGP robot identity
    Given a Passbolt reference naming a resource and field
    And a robot OpenPGP private key and passphrase resolved from secret references
    When the reference is resolved
    Then the decrypted field value is returned

  @REQ-2407 @pending
  Scenario: The OpenBao token bootstraps without a durable on-disk secret
    Given a response-wrapped single-use AppRole SecretID delivered to a memory-backed path
    When the process bootstraps its OpenBao client
    Then it unwraps and logs in without a durable token file on disk
    And a second unwrap of the same wrapping token fails

  @REQ-2408 @pending
  Scenario: The homelab backends are labeled second-tier and are not the default
    Given a fresh installation with no backend selected
    Then no homelab backend is selected by default
    And the documentation labels Vaultwarden and Passbolt as second-tier that relocate rather than eliminate secret-zero

  @REQ-2409 @pending
  Scenario: A hardcoded env default is a violation under enforce
    Given the secret policy is enforce
    And a business secret reference is left to a hardcoded env-scheme default
    When the boot preflight runs
    Then it is treated as a plaintext violation
    And the inline-literal linter no longer blesses the env scheme
