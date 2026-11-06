Feature: Commit-confirmed actuation — the default outcome of a change is revert
  DRAFT (TG-82, awaiting owner sign-off). No scenario below has an oracle yet; the mapping records the
  honest NO-ORACLE state. A mutation survives only if the mechanical verifier positively confirms the
  committed prediction inside the armed window.

  @REQ-2901
  Scenario: A commit-confirmed effect cannot execute without its armed revert
    Given a commit-confirmed-eligible op-class
    When the armed-revert record cannot be durably written
    Then the forward effect is refused before executing anything

  @REQ-2902
  Scenario: Only the mechanical match confirms — an unverifiable post-state does not
    Given an executed commit-confirmed action inside its window
    When the post-state cannot be re-observed
    Then the confirm never arrives and the timer fires the inverse

  @REQ-2903
  Scenario: The fired inverse traverses the full interceptor chain
    Given an armed revert whose window elapsed
    When the inverse executes
    Then it runs the registry rollback argv through every gate and a refusal pages instead of skipping

  @REQ-2904
  Scenario: A class without a registry inverse is never eligible
    Given an op-class with no rollback template
    Then commit-confirmed eligibility is refused at registry load

  @REQ-2905
  Scenario: A staged-canary class cannot execute unconfirmed
    Given an op-class on the staged-canary allowlist
    When its forward effect is requested without an armable revert
    Then the forward is refused

  @REQ-2906
  Scenario: Every commit-confirmed transition is on the ledger and a failed revert pages and trips the breaker
    Given an armed revert whose window elapsed and whose inverse the chain refused
    Then arm, fire, and revert-failed each appended to the ledger bound to the action_id
    And the revert-failed state paged immediately and tripped the mutation breaker

  @REQ-2907
  Scenario: Graduation credits only the confirmed outcome
    Given an executed commit-confirmed action
    When the window elapses and the inverse fires
    Then the run records a deviation and no clean-run credit exists for the forward

  @REQ-2908
  Scenario: An empty pre-action diff is a refused no-op before anything arms
    Given a diff-computing commit-confirmed class whose target already holds the desired state
    Then nothing arms and nothing executes and the refusal is legible
