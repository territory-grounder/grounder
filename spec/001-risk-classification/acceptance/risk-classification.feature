# spec/001 — Three-band risk classification acceptance oracles.
# GREEN scenarios drive the already-implemented core/safety primitives. Scenarios tagged @pending
# specify behavior of the not-yet-built core/risk.Classifier (Phase 2); they are tracked as declared
# debt in acceptance/_test_mapping.json and are skipped by the runner (Tags: "~@pending") until built.
Feature: Three-band risk classification fails closed by construction

  The autonomy band, its zero value, and the never-auto floor are mechanical properties of
  core/safety that no confidence score, tenant policy, or flag can relax.

  @REQ-006
  Scenario: The zero-value band is POLL_PAUSE
    Given a freshly zero-initialised autonomy band
    Then the band is "POLL_PAUSE"

  @REQ-006
  Scenario: An unmatched band value stringifies as POLL_PAUSE
    Given an out-of-range band value 99
    Then the band is "POLL_PAUSE"

  @REQ-004
  Scenario Outline: Irreversible operation classes are on the never-auto floor
    Given the operation class "<opclass>"
    Then the operation is on the never-auto floor

    Examples:
      | opclass           |
      | mkfs              |
      | dropdb            |
      | zpool-destroy     |
      | zfs-destroy       |
      | tofu-destroy      |
      | terraform-destroy |
      | kubectl-delete    |
      | kubectl-drain     |
      | credential-revoke |
      | config-overwrite  |
      | reboot            |
      | jailbreak         |

  @REQ-004
  Scenario: A reversible operation class is not on the never-auto floor
    Given the operation class "restart-service"
    Then the operation is not on the never-auto floor

  @REQ-006
  Scenario: The remediation lane fails closed by default
    Given a freshly zero-initialised fail lane
    Then the lane is the fail-closed remediation lane

  @REQ-001
  Scenario: A low-risk reversible action is classified AUTO
    Given a low-risk reversible action below the org blast-radius threshold on a non-critical host
    When the classifier admits the session
    Then the band is "AUTO" and the proposal is marked AUTO-RESOLVE

  @REQ-002
  Scenario: A reversible-mixed action on a critical host is classified AUTO_NOTICE
    Given a reversible-mixed action on a criticality-tier host
    When the classifier admits the session
    Then the band is "AUTO_NOTICE" and notify_required is true

  @REQ-003
  Scenario: A high-risk irreversible action is classified POLL_PAUSE
    Given a high-risk irreversible action
    When the classifier admits the session
    Then the band is "POLL_PAUSE" and the session does not proceed on timeout

  @REQ-007
  Scenario: An incident with no learned prior is classified POLL_PAUSE
    Given an incident class with no learned prior for its alert rule and host
    When the classifier admits the session
    Then the band is "POLL_PAUSE"

  @REQ-008
  Scenario: An AUTO-RESOLVE lacking bound evidence is downgraded to a poll
    Given the silent_cognition_guard policy is active
    And an AUTO-RESOLVE response with no bound ToolResult evidence
    When the classifier admits the session
    Then the AUTO-RESOLVE marker is stripped and the session is downgraded to a poll

  @REQ-009
  Scenario: A canary-pinned action is forced to POLL_PAUSE
    Given a canary-pinned action that would otherwise be AUTO-eligible
    When the classifier admits the session
    Then the band is "POLL_PAUSE" and the session does not proceed on timeout

  @REQ-010
  Scenario: A leaked credential in the untrusted seed is redacted before it reaches the model or the ledger
    Given untrusted alert text carrying a leaked bearer credential
    When the input screen scrubs the text
    Then the credential is redacted and the redaction is flagged

  @REQ-010
  Scenario: A benign alert body is not mangled by the secret redaction pass
    Given untrusted alert text with only hostnames, IPs, rule names and numbers
    When the input screen scrubs the text
    Then the text passes through unchanged with no redaction
