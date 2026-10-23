Feature: The measurement plane is governed evidence
  The harness produces the v1.0 claim. These scenarios assert the properties that make a published
  number defensible: one bound computation per axis, tests that actually constrain the SQL, populations
  reported with their denominators and exclusions, and like-for-like comparison.

  @REQ-2500
  Scenario: Every measurement surface is bound to this spec in the lockstep lock
    Given the benchmark harness surfaces that compute published axes
    When the lockstep manifest is consulted
    Then every harness surface is bound to spec/025 so an axis cannot be redefined without the hash moving

  @REQ-2501
  Scenario: The measurement SQL is covered by a golden fixture whose expected values are hand-computed
    Given a real database with every migration applied and a hand-built axis fixture
    When the axis aggregate is computed over that fixture
    Then each covered axis equals its hand-computed expected value

  @REQ-2501
  Scenario: A perturbed axis query turns its golden test red
    Given a covered axis and its golden fixture
    When one predicate of that axis query is perturbed
    Then the golden test fails, proving the test constrains the query rather than tracking it

  @REQ-2502 @REQ-2503
  Scenario: An axis that excludes unmeasurable rows reports the exclusion
    Given an axis whose population contains rows it cannot measure
    When the axis is rendered to the scorecard
    Then the report carries the denominator, the excluded count, and the reason for exclusion

  @pending @REQ-2502
  Scenario: A zero-numerator axis is published with its upper bound
    Given an axis with zero observed events over a finite sample
    When the axis is rendered to the scorecard
    Then the report carries the statistical upper bound rather than a bare zero

  @REQ-2504
  Scenario: A comparative aggregate covers only dimensions both systems are scored on
    Given a judged comparison in which one system is not scored on a dimension
    When the comparative aggregate and the head-to-head winner are computed
    Then that dimension is excluded from both and is reported as a unilateral property
