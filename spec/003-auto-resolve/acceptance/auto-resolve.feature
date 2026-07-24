# spec/003 — Per-incident auto-resolve and escalation requeue acceptance oracles.
# No Go implementation exists yet: every scenario is tagged and skipped by the runner
# (Tags: "~@pending"). Each is tracked as declared debt in acceptance/_test_mapping.json until the
# core/reconcile.Reconciler and core/escalation.Queue activities (Phase 2) are built and bound.
Feature: Per-incident auto-resolve closes only on a confirmed clear and requeues unanswered polls

  The reconciler drives a finished session to a terminal decision — closing an incident only after the
  alert condition is verified cleared, recording a per-incident best-outcome outcome, and requeueing an
  unanswered poll as an authenticated re-check rather than closing it silently.

  @REQ-201
  Scenario: An incident closes only after the alert condition is confirmed cleared
    Given a finished band-AUTO session whose host recovery has no orchestrator-captured ToolResult
    When the reconciler evaluates the session for close-out
    Then the incident is not closed and remains open for confirmation

  @REQ-202
  Scenario: A band-AUTO recovered host transitions its ticket to Done
    Given a finished band-AUTO session with a match verdict and confirmed-clear evidence
    When the reconciler evaluates the session for close-out
    Then the recovered host is reconciled and the ticket transitions to "Done"

  @REQ-203
  Scenario: Every close-out records a resolution_type
    Given a session reaching close-out
    When the reconciler writes the close-out record
    Then the record carries a resolution_type drawn from auto_resolved, human_resolved, escalated, or deferred

  @REQ-204
  Scenario: A session with no terminal result leaves the incident open
    Given a session that ended with no terminal result
    When the reconciler evaluates the session for close-out
    Then the incident is left open and transitions to "To Verify"

  @REQ-205
  Scenario: Outcomes are recorded as per-incident best-outcome rows
    Given an alert storm that produced many events for a single incident
    When the reconciler records the outcomes
    Then exactly one per-incident best-outcome row is recorded and the auto-resolve denominator counts the incident once

  @REQ-206
  Scenario: An unanswered poll schedules a delayed re-check in the escalation queue
    Given an approval poll that went unanswered until its session archived
    When the reconciler archives the session as poll_unanswered
    Then a delayed re-check row is scheduled in the escalation queue carrying attempts, status, and eligible_at

  @REQ-207
  Scenario: A re-check with a still-active condition re-escalates and pages the approver graph
    Given a queued re-check whose alert condition is still active
    When the re-check fires
    Then the system re-escalates and pages the approver graph through an authenticated Temporal signal keyed by session

  @REQ-207
  Scenario: A re-check with a recovered condition defers closure to the autocloser
    Given a queued re-check whose alert condition has recovered
    When the re-check fires
    Then the system defers closure to the autocloser and does not page the approver graph

  @REQ-208
  Scenario: Reaching the unanswered-poll cap stands down to a human
    Given a re-check whose per-incident unanswered-poll cap has been reached
    When the re-check fires
    Then the system stands down to the fallback approver rather than retrying autonomously
