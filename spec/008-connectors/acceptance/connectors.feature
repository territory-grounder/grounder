# spec/008 — Day-1 connector fleet acceptance oracles.
# The REQ-821 module-framework scenarios are BOUND — they drive the real capability-scoped registry
# (modules.Registry). Each per-connector scenario remains @pending: it specifies a loadable connector
# module (Phase 1–3) whose implementation does not exist yet, so no step definitions are compiled for it.
# @pending scenarios are tracked as declared debt in acceptance/_test_mapping.json and skipped by the
# runner (Tags: "~@pending") until their owning task builds them.
Feature: The day-1 connector fleet loads behind stable interfaces

  Every external connection the predecessor operates is a shipped, loadable module behind an
  adapters/ interface. A disabled or unregistered module has no execution path (INV-17); actuation
  modules traverse the single Execute chokepoint (INV-21); model output is untrusted typed data (INV-08).

  @REQ-801
  Scenario: The LibreNMS ingest module loads and normalizes a device-down alert
    Given the LibreNMS ingest module is registered and enabled with an NL config row and a GR config row
    When a device-down event arrives from the NL instance
    Then the event is normalized to the canonical triage shape and a triage.requested event is emitted after in-code admission

  @REQ-802
  Scenario: The Prometheus Alertmanager ingest module loads and correlates a firing-then-resolved alert
    Given the Prometheus and Alertmanager ingest module is registered and enabled
    When a firing alert is followed by its resolved alert for the same alertname and target
    Then the transition is correlated to one incident and validated against the explicit grammar

  @REQ-803
  Scenario: The CrowdSec ingest module loads and normalizes a decision event
    Given the CrowdSec ingest module is registered and enabled with a configured LAPI endpoint
    When a scenario decision event arrives
    Then it is normalized to the canonical triage shape and routed through the shared dedup and flap admission path

  @REQ-804
  Scenario: The YouTrack tracker module loads and drives the trigger and audit sink
    Given the YouTrack tracker module is registered and enabled
    When an issue transitions to In Progress
    Then a session is correlated by the issue id and the terminal audit comment is posted as the sink

  @REQ-805
  Scenario: The Jira tracker module loads behind the tracker interface
    Given the Jira reference tracker module is registered
    When it is selected as the active tracker by configuration
    Then it satisfies the same trigger, correlation-key, state-transition, and audit-sink contract as YouTrack

  @REQ-805
  Scenario: The GitHub Issues tracker module loads behind the tracker interface
    Given the GitHub Issues reference tracker module is registered
    When it is selected as the active tracker by configuration
    Then it satisfies the same trigger, correlation-key, state-transition, and audit-sink contract as YouTrack

  @REQ-805
  Scenario: The ServiceNow tracker module loads behind the tracker interface
    Given the ServiceNow reference tracker module is registered
    When it is selected as the active tracker by configuration
    Then it satisfies the same trigger, correlation-key, state-transition, and audit-sink contract as YouTrack

  @REQ-806
  Scenario: The Matrix notifier module loads and binds an approval vote to its decision
    Given the Matrix notifier and approval module is registered and enabled
    When an approver replies to a pending approval poll from an authenticated sender
    Then the vote is bound to the specific pending decision id and the posted body is credential and PII redacted

  @REQ-807
  Scenario: The Twilio SMS notifier module loads and delivers a deduplicated page
    Given the Twilio SMS notifier module is registered and enabled
    When a POLL_PAUSE decision requires an out-of-band page
    Then a page deduplicated by the decision id is delivered and it carries no command-executing content

  @REQ-808
  Scenario: The Mattermost notifier module loads and binds a response to its decision
    Given the Mattermost notifier and approval module is registered and enabled
    When a user responds to an approval prompt from an authenticated identity
    Then the response is bound to the specific pending decision id it answers

  @REQ-809
  Scenario: The Slack notifier module loads behind the notifier interface
    Given the Slack reference notifier module is registered
    When it is selected as the active human channel by configuration
    Then it preserves sender authentication, decision-id binding, and credential and PII redaction

  @REQ-809
  Scenario: The Microsoft Teams notifier module loads behind the notifier interface
    Given the Microsoft Teams reference notifier module is registered
    When it is selected as the active human channel by configuration
    Then it preserves sender authentication, decision-id binding, and credential and PII redaction

  @REQ-809
  Scenario: The SMTP email notifier module loads behind the notifier interface
    Given the SMTP email reference notifier module is registered
    When it is selected as the active human channel by configuration
    Then it preserves sender authentication, decision-id binding, and credential and PII redaction

  @REQ-810
  Scenario: The NetBox CMDB module loads and re-reads an entity by id
    Given the NetBox CMDB module is registered and enabled
    When an ingested payload names a device
    Then the canonical entity is re-read from NetBox by id before dispatch and its changelog is exposed to triage context

  @REQ-811
  Scenario: The SSH actuation module loads and refuses an interactive shell
    Given the SSH actuation module is registered and enabled with a per-agent scoped identity
    When a mutating command is executed as a fixed argv array through the Execute chokepoint
    Then one execution_log row with its rollback command is recorded and no interactive shell or host-key-bypass option is expressible

  @REQ-822
  Scenario: The SSH actuation module keeps its gated mutating path inert while mutation is off
    Given the SSH actuation module is configured with a reversible-op allowlist and the mutation gate off
    When a reversible restart of an allowlisted unit is attempted through the mutating path
    Then the module reports read-only, the restart does not execute, and no command reaches the runner

  @REQ-812
  Scenario: The Kubernetes actuation module loads and clamps delete to the never-auto floor
    Given the Kubernetes actuation module is registered and enabled with a configured cluster context
    When a kubectl delete operation is proposed
    Then it is clamped to the non-configurable never-auto floor regardless of confidence or policy

  @REQ-813
  Scenario: The Proxmox actuation module loads and gates a lifecycle operation behind the enable flag
    Given the Proxmox actuation module is registered with the lifecycle enable flag unset
    When a guest reboot is proposed
    Then the lifecycle operation is refused and reboot is clamped to the non-configurable never-auto floor

  @REQ-814
  Scenario: The MCP tool actuation surface loads and denies an unregistered tool
    Given the MCP tool actuation surface is registered with a set of capability-scoped tools
    When an unregistered tool is invoked
    Then it has no execution path and lifecycle-mutating tools remain behind an explicit enable flag

  @REQ-815
  Scenario: The LiteLLM model-gateway module loads and exposes one OpenAI-compatible endpoint
    Given the bundled LiteLLM model-gateway module is registered and enabled
    When a component resolves a model through the one source-of-truth router
    Then the request is served over one OpenAI-compatible endpoint and real-token usage is recorded with no fabrication

  @REQ-815
  Scenario: The z.ai provider backend resolves through the model-gateway
    Given the z.ai provider backend is configured as the primary ladder rung
    When the gateway routes a request to it and it errors
    Then the configured auto-fallback ladder advances to the next provider and the response is treated as untrusted typed data

  @REQ-815
  Scenario: The DeepSeek provider backend resolves through the model-gateway
    Given the DeepSeek provider backend is configured behind the gateway
    When the gateway routes a request to it
    Then its reasoning response blocks are joined on type text and treated as untrusted typed data

  @REQ-815
  Scenario: The Mistral provider backend resolves through the model-gateway
    Given the Mistral provider backend is configured behind the gateway
    When the gateway routes a request to it
    Then the response is served over the gateway and treated as untrusted typed data

  @REQ-815
  Scenario: The Ollama provider backend resolves through the model-gateway
    Given the Ollama local provider backend is configured behind the gateway
    When the gateway routes a request to it under the local-first cost profile
    Then the response is served over the gateway and treated as untrusted typed data

  @REQ-815
  Scenario: The Anthropic provider backend resolves through the model-gateway
    Given the Anthropic provider backend is configured as a fallback ladder rung
    When the gateway routes a request to it
    Then the response is served over the gateway and treated as untrusted typed data

  @REQ-815
  Scenario: The OpenAI provider backend resolves through the model-gateway
    Given the OpenAI provider backend is configured as a fallback ladder rung
    When the gateway routes a request to it
    Then the response is served over the gateway and treated as untrusted typed data

  @REQ-816
  Scenario: The Prometheus observability module exposes stamped metrics for scrape
    Given the Prometheus observability module is registered and enabled
    When control-plane and per-connector metrics are scraped
    Then each series carries a freshness timestamp and an absent()-guarded staleness metric so a dead writer pages

  @REQ-817
  Scenario: The Grafana observability module provisions dashboards from version control
    Given the Grafana observability module is registered and enabled
    When the control-plane dashboards are provisioned from version-controlled definitions
    Then hand-edited panels are rejected as drift

  @REQ-818
  Scenario: The OpenObserve observability module exports OTLP traces default-on
    Given the OpenObserve observability module is registered and enabled with tracing default-on
    When a session runs to completion
    Then its OTLP traces and logs are exported to the configured endpoint and its trajectory is reconstructable

  @REQ-819
  Scenario: The Langfuse observability module records a per-session trace
    Given the Langfuse observability module is registered and enabled
    When a session invokes the agent loop
    Then a per-session LLM and agent trace keyed by the session id is recorded to the configured endpoint

  @REQ-820
  Scenario: The Healthchecks.io observability module pings a dead-man check on heartbeat
    Given the Healthchecks.io observability module is registered and enabled with a configured dead-man check
    When a scheduled control-plane heartbeat fires
    Then the dead-man check is pinged and a missed heartbeat raises an external alert independent of the internal alert path

  @REQ-823
  Scenario: The syslog-ng investigation connector returns a host's recent logs over a fixed argv SSH read
    Given the syslog-ng investigation connector is configured with an NL server and a GR server and an injected runner
    When the agent requests the recent logs of an NL device host
    Then the read is routed to the NL server as a fixed tail argv and the ASA log lines are returned as a read-only observation

  @REQ-823
  Scenario: The syslog-ng investigation connector refuses a path-traversal host and never reads a file
    Given the syslog-ng investigation connector is configured with an NL server and a GR server and an injected runner
    When the agent requests logs for a host carrying a path-traversal sequence
    Then the connector refuses without leaking a path and the injected runner is never called

  @REQ-821
  Scenario: An unregistered or disabled module has no execution path
    Given a capability-scoped module registry
    When a module is resolved before registration and again while registered but disabled
    Then the registry denies an execution path both times and grants one only once the module is enabled

  @REQ-821
  Scenario: Each surface admits exactly one registered implementation per source type
    Given a capability-scoped module registry with one module registered for a surface and source type
    When a second module is registered for the same surface and source type
    Then the duplicate registration is rejected and a different source type is accepted
