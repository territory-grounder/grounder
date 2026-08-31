# spec/012 — Read-only Runner Temporal workflow acceptance oracles.
# The scenarios drive the REAL RunnerWorkflow in the Temporal in-process test env with in-memory
# governed primitives (CI has no live Temporal server or model). The Runner stops at propose: while
# mutation is off, execute/verify are no-op and the estate is never mutated.
Feature: The read-only Runner workflow drives an incident to a sealed gated proposal and stops at propose

  An ingested incident flows investigate -> classify -> gate to a sealed, classified ActionManifest with
  no mutation; the action_id is threaded unchanged into the sealed manifest; an incident the agent
  cannot propose for ends without an action.

  @REQ-1101 @REQ-1102
  Scenario: An ingested incident flows to a sealed gated proposal with no mutation
    Given a read-only Runner and an ingested incident the agent will propose for
    When the Runner workflow runs to completion
    Then the incident reaches a sealed gated proposal and the estate is not mutated

  @REQ-1103
  Scenario: The action_id is threaded from derivation through the sealed manifest
    Given a read-only Runner and an ingested incident the agent will propose for
    When the Runner workflow runs to completion
    Then the sealed ActionManifest action_id matches the action the workflow derived

  @REQ-1104
  Scenario: An incident the agent cannot propose for ends without an action
    Given a read-only Runner and an incident the agent will not propose for
    When the Runner workflow runs to completion
    Then the session ends without an action and without mutation

  @REQ-1105
  Scenario: An approved poll threads the human authorization into the execute path
    Given a read-only Runner and an incident whose proposal demands a human poll
    When the Runner workflow runs with an approving vote
    Then the approval is ledger-recorded and threaded to execute, and the estate is still not mutated

  @REQ-1105
  Scenario: A denied poll stands the session down without mutation
    Given a read-only Runner and an incident whose proposal demands a human poll
    When the Runner workflow runs with a denying vote
    Then the denial is ledger-recorded and the session stands down without mutation

  @REQ-1105
  Scenario: An unanswered poll times out to deny — never a silent approval
    Given a read-only Runner and an incident whose proposal demands a human poll
    When the Runner workflow runs to completion
    Then the timeout is ledger-recorded as a deny and the session stands down without mutation

  @REQ-1105
  Scenario: A vote bound to a different action is recorded and ignored — it releases nothing
    Given a read-only Runner and an incident whose proposal demands a human poll
    When the Runner workflow runs with an approving vote bound to a different action
    Then the misbound vote is recorded and ignored and the poll still times out to deny

  @REQ-1107
  Scenario: The interceptor's effect leaf is read-only by default and inert while mutation is off
    Given the worker selects its effect-leaf actuator with no SSH host configured
    Then the effect leaf reports read-only and is not an execution recorder

  @REQ-1108 @REQ-1109
  Scenario: A gated execution binds evidence, wires an observer, and constructs the argv
    Given a governed execute activity with mutation enabled for the test and a grounded restart proposal
    When the sealed action is executed through the interceptor
    Then the constructed argv reaches the effect leaf and a mechanical verdict is written from the observed post-state

  @REQ-1108
  Scenario: A mispredicted post-state verifies as a deviation, not a clean match
    Given a governed execute activity with mutation enabled for the test and a grounded restart proposal
    When the sealed action is executed and its post-state surprises the prediction
    Then the verdict is a deviation and the action is not reported clean

  @REQ-1110
  Scenario: Precedent ranked by both channels fuses above single-channel precedent
    Given a precedent corpus with a lexical ranking and an embedded semantic ranking
    When precedent is retrieved for the incident through the fused retriever
    Then the precedent ranked in both channels outranks every single-channel precedent

  @REQ-1110
  Scenario: A semantic match below the similarity floor never enters the seed
    Given a precedent corpus whose only semantic neighbor scores below the similarity floor
    When precedent is retrieved for the incident through the fused retriever
    Then the retrieval equals the lexical ranking exactly

  @REQ-1111
  Scenario: Retrieval without an embedding model is exactly the lexical behavior
    Given a precedent corpus and no embedding model configured
    When precedent is retrieved for the incident through the fused retriever
    Then the retrieval equals the lexical ranking exactly

  @REQ-1111
  Scenario: An embedding outage degrades that query to the lexical ranking
    Given a precedent corpus whose embedder fails
    When precedent is retrieved for the incident through the fused retriever
    Then the retrieval equals the lexical ranking exactly and the degrade is logged

  @REQ-1112
  Scenario: The seed wraps each block in a typed envelope and a forged guidance delimiter is neutralized
    Given a read-only Runner and an incident whose alert text forges a trusted-guidance delimiter
    When the investigation seed is composed
    Then only the composer's behavioral_guidance block is a trusted boundary and every other block is delimited untrusted data

  @REQ-1113
  Scenario: A finished session drives a terminal ticket close-out on the governance ledger
    Given a read-only Runner with a tracker and an escalation re-check lane and an incident whose proposal demands a human poll
    When the Runner workflow runs to completion
    Then the terminal reconcile transitions the ticket and records a close-out on the governance ledger

  @REQ-1113
  Scenario: A confirmed-clear auto session is closed out to Done
    Given a terminal reconcile of a confirmed-clear auto session
    When the session is reconciled
    Then the incident is closed out to Done through the tracker

  @REQ-1113
  Scenario: A terminal reconcile never auto-closes an executed action whose post-state deviated
    Given a terminal reconcile of an executed action whose post-state deviated
    When the session is reconciled
    Then the incident is left open to verify and is not auto-closed

  @REQ-1115
  Scenario: An unanswered poll hands the orphaned incident off to the escalation re-check lane
    Given a read-only Runner with a tracker and an escalation re-check lane and an incident whose proposal demands a human poll
    When the Runner workflow runs to completion
    Then the orphaned poll is requeued into the escalation re-check lane

  @REQ-1114
  Scenario: The scheduled FireDue cron fires every due escalation re-check
    Given a scheduled FireDue cron over an escalation lane with a due re-check
    When the FireDue cron workflow runs
    Then the due re-check is fired and the run completes

  @REQ-1114
  Scenario: A FireDue error is captured and never crashes the cron run
    Given a scheduled FireDue cron whose escalation lane errors
    When the FireDue cron workflow runs
    Then the run completes with the error captured and the worker is not crashed

  @REQ-1116
  Scenario: A persistently failing pipeline activity is retried a bounded number of times then surfaces
    Given a read-only Runner whose first pipeline activity fails every attempt
    When the Runner workflow runs against the failing activity
    Then the failing activity is retried at most its bounded maximum and the session surfaces the failure rather than looping

  @REQ-1117
  Scenario: A session that exhausts its wall-clock budget stops to the terminal human-handoff
    Given a read-only Runner with a tracker and an escalation re-check lane and an incident whose proposal demands a human poll
    When the Runner workflow runs to completion under a short wall-clock budget
    Then the session stops budget-exceeded and hands the orphaned incident to the escalation re-check lane without mutation
