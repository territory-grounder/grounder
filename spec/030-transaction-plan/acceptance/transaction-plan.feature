Feature: Transaction plans — approved once, all-or-nothing
  Owner-ruled governance (2026-08-22): one approval for the whole plan; any step failure
  auto-compensates the completed steps; compensation failure pages and trips, never hides.

  @REQ-3001
  Scenario: No model token can shape a plan
    Given a proposal whose op-class matches a declared recipe
    When the plan is composed
    Then every step comes verbatim from the compiled registry
    And a proposal naming steps, order or extra targets changes nothing about the plan

  @REQ-3002
  Scenario: The vote sees every step and binds the plan identity
    Given a composed three-step plan
    When the poll is presented
    Then all three steps and their compensations are rendered before the vote
    And the single approval binds the content-addressed plan_id
    And a workflow whose re-derived plan_id mismatches refuses to execute

  @REQ-3003
  Scenario: A plan is never partially admissible
    Given a plan whose second step classifies harder than the presented floor
    When classification runs for all steps before execution
    Then the whole plan refuses before step one executes

  @REQ-3004
  Scenario: A mid-plan failure compensates everything already done
    Given an approved three-step plan whose third step fails at the effect
    When the plan workflow handles the failure
    Then the compensations of steps two and one execute in reverse order through the full chain
    And the plan terminal is "reverted"

  @REQ-3005
  Scenario: A failing compensation pages and trips instead of pretending
    Given a reverting plan whose compensation for step one fails
    When the compensation error surfaces
    Then compensation stops, the page fires and the mutation breaker trips
    And the record names exactly which steps remain applied

  @REQ-3006
  Scenario: The ledger tells the plan's whole story
    Given a plan that executed two steps and reverted
    When the governance ledger is read for the plan_id
    Then it holds the proposal, the one approval, both executions, both compensations and the terminal
    And every entry carries the plan_id and that step's action_id

  @REQ-3007
  Scenario: The shipped lane is inert three ways
    Given zero declared recipes
    When any session runs
    Then no plan is ever composed and the single-action path is byte-identical
    And with a recipe declared but mutation at Shadow nothing executes
    And a recipe containing an ungraduated op-class refuses composition
