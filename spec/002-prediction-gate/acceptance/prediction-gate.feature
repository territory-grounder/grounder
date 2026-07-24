# spec/002 — Fail-closed prediction-gate acceptance oracles.
# This is the remediation lane: it fails CLOSED. Every scenario is tagged — the Go
# implementations (core/predict.PredictionGate, core/verify.Verifier in Phase 2) are not bound to
# these oracles are bound to core/predict + core/verify + core/manifest. Pending items are tracked
# as declared debt in acceptance/_test_mapping.json and skipped by the runner (Tags: "~@pending").
Feature: Fail-closed prediction gate binds prediction, verdict, and action identity

  Before any approval poll opens, a machine consequence prediction is committed outside the LLM and
  bound to the action; after execution only a deterministic verifier writes the verdict, and a
  deviation from the committed prediction can never auto-resolve.

  @REQ-101
  Scenario: A prediction is committed before the approval poll starts
    Given a remediation proposal with a plan_hash and a machine-computed cascade prediction
    When the remediation workflow reaches the approval stage
    Then a plan_hash-keyed prediction row exists in the append-only prediction store
    And it was committed before the approval poll activity started

  @REQ-102
  Scenario: A proposal with no committed prediction is denied
    Given a remediation proposal with no committed prediction
    When the prediction gate evaluates the approval poll
    Then the approval poll is denied by default

  @REQ-102
  Scenario: An approval poll cannot be built without a gated prediction
    Given a proposal that is not a GatedProposal produced by the PredictionGate
    When the caller attempts to build an approval poll from it
    Then the approval poll cannot be constructed

  @REQ-102
  Scenario: The sole ParseProposal entry point rejects every second grammar
    Given a battery of model responses that are markdown, a sentinel marker, an alternate grammar, or malformed JSON
    When ParseProposal parses each response
    Then only a schema-valid tool-call is accepted and every other response fails closed

  @REQ-105
  Scenario: Analysis-only mode records the prediction without gating the approval
    Given the prediction gate is in analysis-only mode
    When a remediation proposal is evaluated
    Then the prediction and a shadow verdict are recorded
    And the approval is not blocked on the prediction

  @REQ-103
  Scenario: The mechanical verdict is written only by the deterministic verifier
    Given an executed action with a committed prediction and an observed alert set
    When the verdict is computed
    Then the match, partial, or deviation verdict is written by the deterministic verifier
    And it equals the mechanical diff of observed against predicted

  @REQ-103
  Scenario: The acting model has no write path to the verdict columns
    Given an executed action whose session role is the acting model role
    When the acting model attempts to write a verdict column
    Then the write is rejected because the model and session roles hold no UPDATE or DELETE grant

  @REQ-103a
  Scenario: The verifier returns a typed verdict detail whose enum matches the bare verdict
    Given an executed action with a committed prediction and a mixed observed alert set
    When the typed verdict detail is computed in one pass
    Then the detail enum equals the bare mechanical verdict for the same inputs
    And the detail lists the surprise hosts and the rule mismatches that produced it
    And a deviation detail is never auto-resolvable

  @REQ-104
  Scenario: A deviation verdict blocks auto-resolution
    Given a completed action whose mechanical verdict is deviation
    When the reconciler evaluates auto-resolution
    Then auto-resolution is refused regardless of band or confidence
    And the session routes to POLL_PAUSE and the approver graph

  @REQ-102b
  Scenario: The action_id is threaded unchanged through predict approve execute and verify
    Given an ActionManifest sealed around an action with a content-hashed action_id
    When each of the predict, approve, execute, and verify stages asserts the manifest
    Then every stage re-derives the same action_id and the assertion passes

  @REQ-102b
  Scenario: A mid-session action change mints a new action_id and forces a re-gate
    Given a sealed ActionManifest with a committed prediction and approval
    When the bound action is changed mid-session
    Then a new action_id is derived that does not match the prior authorization
    And the prior prediction and approval are invalidated and the action re-enters the gate

  @REQ-102b
  Scenario: The manifest action_id changes when any action field changes
    Given a canonical action
    When a field of the action is changed
    Then the action_id of the changed action differs from the original

  @REQ-102b
  Scenario: A sealed manifest asserts its own action_id and rejects a foreign one
    Given a sealed ActionManifest built from a canonical action
    Then asserting the manifest against its own action_id passes
    And asserting the manifest against a foreign action_id fails
