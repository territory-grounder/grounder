# spec/013 — Wired-by-construction actuation interceptor acceptance oracles.
# Every governed mutation passes through one chokepoint reachable only via the chain; every failed check
# refuses loud and records it; mutation is enabled only through the proven wired gate. The scenarios
# drive the real core/actuate.Interceptor with an in-memory actuator + ledger.
Feature: The actuation interceptor is the single wired-by-construction chokepoint for every mutation

  A mutating side effect can only execute through admission -> never-auto floor -> structure gate ->
  evidence -> execute -> verify -> audit; any failed check refuses loud (never observe-only); and
  autonomous mutation can be enabled only after the chain is proven wired.

  @REQ-1203
  Scenario: A mutating request is refused while mutation is off
    Given a wired interceptor with mutation off
    When a fully-admissible mutating request is intercepted
    Then the request is refused and nothing is executed

  @REQ-1202
  Scenario: An unwired interception chain fails loud and does not execute
    Given an interceptor missing a governed collaborator
    When a mutating request is intercepted
    Then the interceptor fails loud and nothing is executed

  @REQ-1203
  Scenario: A never-auto floor op is refused at the adapter even with mutation on
    Given a wired interceptor with mutation enabled
    When a never-auto floor operation is intercepted
    Then the request is refused and nothing is executed

  @REQ-1219
  Scenario: A single-host-bound effect leaf refuses an action targeting a different host
    Given a wired interceptor with mutation enabled
    When a mutating action targeting a different host than the single-host-bound effect leaf is intercepted
    Then the request is refused and nothing is executed

  @REQ-1204
  Scenario: An ungated action is refused
    Given a wired interceptor with mutation enabled
    When a mutating action with no committed prediction is intercepted
    Then the request is refused and nothing is executed

  @REQ-1204
  Scenario: An action_id mismatch is refused
    Given a wired interceptor with mutation enabled
    When a mutating action whose bound action changed is intercepted
    Then the request is refused and nothing is executed

  @REQ-1205
  Scenario: An action with no bound evidence is refused
    Given a wired interceptor with mutation enabled
    When a mutating action citing no orchestrator-captured evidence is intercepted
    Then the request is refused and nothing is executed

  @REQ-1206
  Scenario: Mutation is enabled only through the proven wired gate
    Given a mutation gate that is off
    When enabling mutation is attempted on an unwired chain and then on a wired chain
    Then the unwired attempt is refused and the wired attempt enables mutation

  @REQ-1201 @REQ-1207
  Scenario: A fully-admissible governed actuation executes, verifies, and audits
    Given a wired interceptor with mutation enabled
    When a fully-admissible mutating request is intercepted
    Then the action executes once, a mechanical verdict is written, and the decision is audited

  @REQ-1208
  Scenario: A mutating action with no post-execution observer is refused before executing
    Given a wired interceptor with mutation enabled
    When a mutating action with no post-execution observer is intercepted
    Then the request is refused and nothing is executed

  @REQ-1208
  Scenario: A mispredicted post-state verifies as a deviation
    Given a wired interceptor with mutation enabled
    When a mutating action whose post-state surprises its prediction is intercepted
    Then the action executes and the verdict is a deviation

  @REQ-1209
  Scenario: An executed mutation records an execution_log bound to the action id
    Given a wired interceptor with mutation enabled
    When a fully-admissible mutating request is intercepted
    Then an execution_log bound to the action id is recorded

  @REQ-1210
  Scenario: A breaker trip in one worker force-Shadows a sibling worker sharing the durable store
    Given two workers whose armed breakers share one durable breaker store
    When a deviation trips the first worker's breaker
    Then the second worker refuses a fully-admissible mutation and drops to read-only

  # --- Policy-authorize: the interceptor honors an approve verdict as "route to a human vote" — with the vote
  # on file it PROCEEDS (so an ungraduated op-class can earn its clean runs toward auto), without it it refuses,
  # a deny never lifts, and the never-auto floor stays inviolable. (Verdict semantics own: spec/015.)

  @REQ-1216
  Scenario: A policy approve verdict with a recorded human approval executes
    Given a wired interceptor with mutation enabled and a policy engine that resolves approve
    When a reversible poll-band mutating request with a recorded human approval is intercepted
    Then the action executes once, a mechanical verdict is written, and the decision is audited

  @REQ-1216
  Scenario: A policy approve verdict with no recorded approval is refused
    Given a wired interceptor with mutation enabled and a policy engine that resolves approve
    When a mutating request with no recorded human approval is intercepted
    Then the request is refused and nothing is executed

  @REQ-1216
  Scenario: A policy deny verdict is refused even with a recorded human approval
    Given a wired interceptor with mutation enabled and a policy engine that resolves deny
    When a reversible poll-band mutating request with a recorded human approval is intercepted
    Then the request is refused and nothing is executed

  @REQ-1203
  Scenario: The never-auto floor refuses an irreversible op even when approved and policy-auto
    Given a wired interceptor with mutation enabled and a policy engine that resolves auto
    When an irreversible floor-class request with a recorded human approval is intercepted
    Then the request is refused and nothing is executed

  # --- The cost/budget spend guard: the $-ceiling breaker, the INDEPENDENT sibling of the mutation breaker.
  # Every scenario drives the real core/cost.Accountant with an in-memory store + a real mode chokepoint.

  @REQ-1211
  Scenario: Cost accrues for each model completion into the daily and session accumulators
    Given a cost breaker with a per-1k rate and no budget
    When two model completions are metered for a session
    Then the daily and session accumulators reflect the accrued spend and nothing is force-Shadowed

  @REQ-1212
  Scenario: Exceeding the daily budget trips the cost breaker and force-Shadows
    Given a cost breaker with a small daily budget
    When metered completions push the daily spend over the budget
    Then the cost breaker trips, forces the mode to Shadow, and records a cost trip to the ledger

  @REQ-1212
  Scenario: Exceeding a session ceiling trips the cost breaker and force-Shadows
    Given a cost breaker with a small session ceiling
    When a metered completion pushes one session over the ceiling
    Then the cost breaker trips and forces the mode to Shadow

  @REQ-1213
  Scenario: A cost breaker trip force-Shadows a sibling worker sharing the durable store
    Given two cost breakers sharing one durable cost store
    When the first exceeds its budget and the sibling meters its next completion
    Then the sibling forces its own mode to Shadow from the shared open state

  @REQ-1214
  Scenario: A zero budget disables cost enforcement
    Given a cost breaker with rates but no budget or ceiling
    When a very large spend is metered
    Then the cost breaker never trips and the mode stays actuating

  @REQ-1215
  Scenario: An unreadable cost store fails open and does not halt operations
    Given a cost breaker whose store cannot be read
    When a metered completion would otherwise exceed a tiny budget
    Then the cost breaker fails open, does not force Shadow, and logs the error
