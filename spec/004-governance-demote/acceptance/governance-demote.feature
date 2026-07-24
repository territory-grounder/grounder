# spec/004 — Governance auto-demote + judge-death detection acceptance oracles.
# Every scenario is: the core/governance worker + monitor (Phase 2-3) do not exist yet.
# They are tracked as declared debt in acceptance/_test_mapping.json and are skipped by the runner
# (Tags: "~@pending") until the owning tasks in tasks.json are implemented.
Feature: Governance auto-demote and judge-death detection are self-monitoring and reversible

  A genuine repeat-offender tuple is demoted to analysis-only by a metric+audit+expiry circuit-breaker
  with no manual review, and the local judge's liveness is measured from tables the judge cannot write.

  @REQ-301
  Scenario: A genuine repeat-offender tuple is auto-demoted to analysis-only
    Given a tuple of host and alert rule classified a genuine repeat-offender
    When the governance-metrics worker runs
    Then the tuple is demoted to analysis-only
    And Tier-1 suppression escalates the tuple instead of suppressing or auto-resolving it

  @REQ-302
  Scenario: A tuple recurring three times in thirty days is classified a demote candidate
    Given a tuple of host and alert rule that recurred three times within thirty days
    When the governance-metrics worker runs
    Then the tuple is classified as a demote candidate

  @REQ-302
  Scenario: A tuple recurring twice is not classified a demote candidate
    Given a tuple of host and alert rule that recurred twice within thirty days
    When the governance-metrics worker runs
    Then the tuple is not classified as a demote candidate

  @REQ-303
  Scenario: An intentional known-transient tuple is excluded from demotion
    Given a demote candidate tuple tagged as an intentional known-transient for the organization
    When the governance-metrics worker runs
    Then the tuple is excluded from demotion

  @REQ-304
  Scenario: A demotion auto-expires after thirty days
    Given a demotion policy row written thirty-one days ago
    When the governance-metrics worker runs
    Then the read path treats the demotion as expired and the tuple is eligible again
    And no manual review was required

  @REQ-305
  Scenario: The judge-liveness monitor computes the judged fraction from judge-independent tables
    Given ten recently-ended sessions of which six carry a real local judgment
    When the judge-liveness monitor runs
    Then the judged fraction denominator is drawn from tables the judge does not write
    And the judged fraction is reported as zero point six

  @REQ-305
  Scenario: A non-recent session is excluded from the judged fraction
    Given a session that ended before the recency window
    When the judge-liveness monitor runs
    Then the non-recent session is excluded from the judged fraction

  @REQ-306
  Scenario: A judged fraction below one half over more than three sessions raises a judge-death warning
    Given more than three eligible recently-ended sessions and a judged fraction below one half
    When the judge-liveness monitor runs
    Then a judge-death warning is raised and routed through the escalation module

  @REQ-306
  Scenario: A judged fraction below one half with three or fewer eligible sessions raises no warning
    Given three or fewer eligible recently-ended sessions and a judged fraction below one half
    When the judge-liveness monitor runs
    Then no judge-death warning is raised

  @REQ-301
  Scenario: A governance demotion decision is appended to the immutable audit spine
    Given a genuine repeat-offender tuple that the worker demotes
    When the demotion decision is written
    Then the decision is appended to the immutable hash-chained audit spine

  @REQ-305
  Scenario: A raw judged transcript is purgeable while the judged fraction fact is retained
    Given a right-to-erasure purge of raw judged transcripts
    When the purge runs
    Then the raw transcripts are removed from the purgeable operational store
    And the recorded judged fraction fact remains on the immutable audit spine
