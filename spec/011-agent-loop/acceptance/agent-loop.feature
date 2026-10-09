# spec/011 — Native Go agent loop acceptance oracles.
# The agent calls the LiteLLM gateway (no subprocess), uses only read-only tools, and emits its
# proposal through the single ParseProposal grammar. No model token becomes control flow; every
# unparseable/unknown path fails closed. All scenarios drive the real agent package with a scripted
# in-memory model (CI has no live LLM).
Feature: The native Go agent loop investigates and proposes without any model token becoming control flow

  The agent reaches the model only through the gateway, dispatches tools by an exact allowlist lookup
  (never by executing model text), fails closed on any unparseable output, escalates on low confidence,
  and is bounded by cycle limits.

  @REQ-1001 @REQ-1006
  Scenario: The agent drives a read-only tool then emits a proposal via ParseProposal
    Given an agent with a scripted model that calls a read-only tool then proposes at high confidence
    When the agent runs
    Then it captures the read-only tool result and emits a schema-valid proposal

  @REQ-1003
  Scenario: A write tool is structurally withheld from the agent
    Given a read-only tool set
    When a mutating tool is registered
    Then registration is refused and the write tool is absent from the set

  @REQ-1002
  Scenario: A low-confidence turn stops and routes to a poll
    Given an agent whose scripted model proposes below the stop threshold
    When the agent runs
    Then the agent stops without a usable proposal

  @REQ-1002
  Scenario: A mid-confidence proposal escalates to a human poll
    Given an agent whose scripted model proposes between the stop and escalate thresholds
    When the agent runs
    Then the agent escalates the proposal to a poll

  @REQ-1005
  Scenario: An unparseable model output fails closed with no second grammar
    Given an agent whose scripted model returns markdown instead of a typed directive
    When the agent runs
    Then the agent stops and no looser grammar accepts the output

  @REQ-1001
  Scenario: An injection tool name is never executed
    Given an agent whose scripted model names a tool with shell metacharacters
    When the agent runs
    Then the unknown tool name is not executed and the agent stops

  @REQ-1004
  Scenario: The cycle hard-halt limit bounds the loop
    Given an agent whose scripted model never proposes and a low hard-halt limit
    When the agent runs
    Then the agent hard-halts at the cycle limit

  @REQ-1009
  Scenario: A tool publishes an ACI schema that renders into the preamble and screens bad arguments
    Given a read-only tool that publishes an ACI description and typed parameter schema
    When the agent runs against a model that first calls the tool with a missing required argument then correctly
    Then the tool catalog renders the tool description and its typed parameters
    And the invalid call is refused as an actionable tool-error, never executed, and the agent recovers to a proposal

  @REQ-1010
  Scenario: A tool error is recoverable and does not abort the session
    Given a read-only tool whose invocation errors and a second working tool
    When the agent runs
    Then the tool error becomes an observation and the agent tries the other tool and proposes

  @REQ-1011
  Scenario: A model thought is recorded as data and never routes dispatch
    Given an agent whose model emits a tool directive whose thought demands a stop
    When the agent runs
    Then the tool still runs and the proposal lands, and the thought is recorded but never routed dispatch

  @REQ-1012
  Scenario: A hostile tool result is neutralized in the observation while a benign result and its id pass byte-clean
    Given an agent whose tools return one hostile result then a benign one
    When the agent runs
    Then the hostile observation the model sees is neutralized and flagged while the benign observation and every observation id pass byte-clean
