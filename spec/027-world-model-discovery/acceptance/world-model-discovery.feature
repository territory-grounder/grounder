Feature: Auto-drafted world model — the admin reviews, never authors
  Discovery drafts the estate manifest; adoption is a one-click audited act; approved entries feed
  the allowlists while leaf default-deny enforcement stays byte-untouched. RED mutation controls are
  named per oracle in design.md.

  @REQ-2701 @pending
  Scenario: Discovery drafts entries with source provenance and table confidence
    Given fake systemd and docker transports behind the discovery EdgeSources
    When discovery runs
    Then manifest entries exist in draft status with per-source provenance and table confidence

  @REQ-2701 @pending
  Scenario: An unknown entity type from a corrupted source is loud-rejected
    Given a discovery source emitting an entity type outside the closed vocabulary
    When discovery runs
    Then the entry is rejected with a loud error and nothing is inserted

  @REQ-2702
  Scenario: Adopting an entry appends the ledger row before the row update
    Given a draft manifest entry
    When the operator adopts it with a rationale
    Then the manifest:adopt ledger row precedes the row update and carries the rationale

  @REQ-2703
  Scenario: Reject without a rationale is refused
    Given a draft manifest entry
    When the operator posts reject with an empty rationale
    Then the request is refused with a client error and the entry is unchanged

  @REQ-2703 @pending
  Scenario: The manifest view renders diffs and honors server-computed caller permissions
    Given the served console with draft and approved manifest entries
    When the manifest view renders
    Then the draft-versus-approved diff is visible and adopt controls honor caller_can_act false

  @REQ-2704
  Scenario: A non-adopted unit is refused at the leaf and the same effect passes after adopt
    Given an ssh effect targeting a unit absent from every allowlist source
    When the effect executes before and after a one-click adopt of that unit
    Then the pre-adopt execution is refused at the leaf and the post-adopt execution passes the leaf gate

  @REQ-2705 @pending
  Scenario: Drift marks disappeared entries stale and drafts new entries without auto-retiring
    Given an approved manifest and a mutated fake source
    When the re-discovery cron runs
    Then disappeared entries become stale and never retired, new entries become drafts, and manifest:drift is ledgered

  @REQ-2705 @pending
  Scenario: A failing source contributes nothing and is reported loudly
    Given one discovery source whose transport errors
    When the re-discovery cron runs
    Then that source's entries are unchanged, the error is reported per source, and other sources proceed

  @REQ-2706 @pending
  Scenario: Adoption never lowers confidence and learned contributions stay capped
    Given a draft entry whose source confidence exceeds an existing lower-confidence claim
    When the entry is adopted
    Then the materialized confidence follows the MAX-ratchet and learned-tier caps

  @REQ-2707 @pending
  Scenario: A ratified class without adopted targets polls but cannot touch the host
    Given a ratified op-class whose target unit is not an adopted manifest entry
    When an action for that class is approved
    Then the poll opens normally and the effect is refused at the leaf default-deny gate
