# spec/016 — Credential / Identity Engine acceptance oracles.
# Nothing is built yet: every scenario specifies behavior of the not-yet-built core/credential.Engine
# (Phase 2; read-only resolution + sync is Phase-1-safe) and is tagged @pending, tracked as declared debt
# in acceptance/_test_mapping.json and skipped by the runner (Tags: "~@pending") until the owning task
# lands. The engine is the authentication sibling of the policy engine (spec/015); it never lifts the
# constitutional mechanical never-auto floor (INV-09) and never falls back to a default identity.
Feature: The credential engine resolves the right per-target identity and syncs it from the estate's platforms

  Resolution is config-not-code and fails closed (an unresolved target is refused, never a default
  identity); credentials are sealed at rest as SecretRefs; a unified sync framework imports identities
  from AWX/Ansible/Semaphore/OpenBao/Vault (machine plane) and LDAP/OIDC (human plane) rather than
  hand-entering them; the two planes are kept apart; and the engine composes with the policy engine's
  authorization at actuation.

  @REQ-1600
  Scenario: A target resolves to exactly one credential bundle from config data
    Given operator-declared resolver config mapping host glob group and device-class to bundles
    When the engine resolves a target
    Then exactly one credential bundle is returned from the config data with no code change

  @REQ-1601
  Scenario: A credential bundle is constructible only with all required fields
    Given a credential bundle with an unset required field
    When the bundle is constructed
    Then construction is a type error and no blank-identity bundle exists

  @REQ-1602
  Scenario: An unresolved target is refused with no default identity
    Given a target that no resolver rule and no synced source covers
    When the engine resolves the target
    Then the engine refuses and the target is neither investigable nor actuatable and no default or last-used identity is returned

  @REQ-1602
  Scenario: The fail-closed property holds under an empty or partially-synced store
    Given an empty or partially-synced credential store
    When the engine resolves a target not covered by the synced subset
    Then the engine fails closed and refuses rather than falling open to a default identity

  @REQ-1603
  Scenario: Every credential value is a SecretRef resolved at runtime and no plaintext is stored
    Given a resolver config and credential store
    When a credential bundle is resolved
    Then every secret-bearing field is a SecretRef resolved through the sealed store at runtime and no plaintext credential value appears in config the store or any exportable artifact

  @REQ-1604 @pending
  Scenario: Identity is resolved after a non-deny policy verdict and before execute
    Given a classified action that the policy engine has authorized with a non-deny verdict
    When the actuation chokepoint reaches the target
    Then the credential engine resolves the identity after authorization and before the interceptor executes

  @REQ-1605
  Scenario: The resolver matches on the same estate object-model as the policy engine
    Given one estate object-model of host glob group device-class and inventory
    When the credential resolver and the policy engine match a target
    Then both key off the same object-model and no second inventory grammar is defined

  @REQ-1606
  Scenario: A multi-rule target resolves by most-specific-wins and an equal-specificity conflict refuses
    Given a target matched by more than one resolver rule
    When the engine selects a bundle
    Then it applies most-specific-wins precedence and an equal-specificity conflict fails closed instead of choosing arbitrarily

  @REQ-1607
  Scenario: A source syncs read-only into the native store on schedule and on demand
    Given a configured CredentialSource
    When Sync runs on the operator schedule and on demand
    Then the source performs a read-only pull into the native store and re-reads each object from its system-of-record by id

  @REQ-1608
  Scenario: A repeated sync of unchanged data converges with no duplicate or orphan
    Given a source that has already synced upstream data that has not changed
    When Sync runs again
    Then the store converges to the same state keyed by source and native object id with no duplicated identity and no orphaned bundle

  @REQ-1609
  Scenario: A target in multiple sources resolves by source precedence and records shadowed sources
    Given a target present in more than one synced source
    When the engine resolves the target
    Then it applies the operator-declared source precedence records the winning and shadowed sources and fails closed when the precedence does not disambiguate

  @REQ-1610
  Scenario: A target with no synced source resolves from the native-store fallback
    Given a target that no synced source covers but the native store does
    When the engine resolves the target
    Then it resolves from the native store as the standalone fallback with zero third-party dependency

  @REQ-1611
  Scenario: A machine-plane source cannot populate approver identity
    Given a machine-plane source such as AWX or Vault
    When the source syncs
    Then it populates only host credential bundles and never an approver identity

  @REQ-1611
  Scenario: An approver-plane source cannot populate a host credential bundle
    Given an approver-plane source such as LDAP or OIDC
    When the source syncs
    Then it populates only approver identities and never a host credential bundle

  @REQ-1612
  Scenario: The AWX Ansible Semaphore source pulls inventory and credentials read-only with no subprocess
    Given an AWX Ansible or Semaphore platform
    When the source syncs through its native Go client
    Then it pulls inventory and per-target credentials read-only and spawns no subprocess

  @REQ-1613
  Scenario: The vault bao SecretRef scheme reads KV v2 read-only and fails closed on unreachable or denied or expired
    Given a vault or bao SecretRef backed by OpenBao or HashiCorp Vault
    When the engine resolves the reference
    Then it performs a read-only KV v2 read under AppRole JWT or Kubernetes auth and fails closed with no default credential when unreachable denied or expired

  @REQ-1619
  Scenario: The oidc SecretRef scheme mints a machine-plane client-credentials token and fails closed on unreachable or denied or untrusted-TLS
    Given an oidc SecretRef backed by an OIDC provider token endpoint
    When the engine mints the token
    Then it mints a machine-plane Bearer token via the client-credentials grant over verified TLS caches it by expires_in and fails closed with no default token when unreachable denied or the server certificate does not verify

  @REQ-1614
  Scenario: The LDAP OIDC source pulls approver identities read-only and never writes a host bundle
    Given a configured LDAP or OIDC provider
    When the source syncs
    Then it pulls users and groups read-only into the human approver plane for spec/015 approve_by and writes no host credential bundle

  @REQ-1615
  Scenario: Each source records last-synced and drift
    Given a source that has run Sync
    When the console reads the source status
    Then last-synced and the drift of upstream objects added changed or removed are recorded and surfaced

  @REQ-1616 @pending
  Scenario: Read-only fact-gathering is a Phase-1-safe sensor and playbook actuation routes through the mode chokepoint
    Given the AWX Ansible Semaphore source used as an effect channel
    When TG gathers facts read-only and later runs a playbook or job template
    Then fact-gathering is a Phase-1-safe sensor and the actuation routes through the mode chokepoint and the constitutional never-auto floor

  @REQ-1617 @pending
  Scenario: Every resolution and sync is appended to the governance ledger with no secret value
    Given the engine resolves a target and a source runs Sync
    When the decision and the sync run are produced
    Then one credential_resolution row and one credential_sync_run row carrying only non-secret identity metadata are appended to the tamper-evident ledger

  @REQ-1617 @pending
  Scenario: The credential tables are append-only with no runtime UPDATE or DELETE
    Given the credential persistence migration
    When the runtime database role attempts to update or delete a credential_resolution or credential_sync_run row
    Then the operation is denied by the grants

  @REQ-1617 @pending
  Scenario: A source is admitted only when its connector adapter is registered at startup
    Given a source whose connector adapter is not compiled in and registered
    When the engine loads sources at startup
    Then the unregistered source is not admitted and no runtime activation of an unregistered backend is possible

  @REQ-1615 @pending
  Scenario: The console renders per-source last-synced and drift
    Given the console credentials surface
    When the operator views the configured sources
    Then each source renders its last-synced time and its drift indicator

  @REQ-1618 @pending
  Scenario: The console configures tests schedules and syncs each source on demand
    Given the first-class console credential surface
    When the operator adds tests schedules and triggers Sync now on a source
    Then the source is configured tested scheduled and synced on demand and its last-synced and drift are shown

  @REQ-1618 @pending
  Scenario: The console renders the per-target credential map with source and precedence
    Given resolved bundles from multiple sources and the native fallback
    When the operator views the per-target credential map
    Then each target shows its bundle the winning source the precedence and the native-store fallback entry

  @REQ-1618 @pending
  Scenario: The console coverage view answers whether TG can reach a target
    Given the console coverage view
    When the operator inspects a target
    Then the view shows whether the target has a resolved credential or refuses paired with the policy packet-tracer answer for whether TG may act

  @REQ-1618 @pending
  Scenario: The console object-groups editor is shared with the policy engine
    Given the shared estate object-groups editor
    When the operator edits an object-group
    Then the same object-group is consumed by both the credential resolver and the policy engine with no second definition

  @REQ-1618 @pending
  Scenario: A secret value is write-only in the console and never echoed
    Given the console credential surface
    When the operator enters a secret value and later views the credential
    Then the secret is stored write-only never echoed back and the edit is appended to the tamper-evident ledger
