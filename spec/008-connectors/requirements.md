<!-- spec/008 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/008 — Day-1 connector fleet (loadable integration modules)

**Owning behavior family:** the module system (see [`docs/PRODUCT.md`](../../docs/PRODUCT.md) §6, [`docs/CAPABILITY-INVENTORY.md`](../../docs/CAPABILITY-INVENTORY.md) Area 5).
**Constitution / invariants:** INV-01, INV-02, INV-04, INV-05, INV-06, INV-07, INV-08, INV-09, INV-13, INV-17, INV-18, INV-19, INV-21.
**Phase:** interface set lands P1; the day-1 backends land P1–P3. **Status:** Approved.

This spec captures the **operator directive** recorded in [`docs/CONNECTOR-INVENTORY.md`](../../docs/CONNECTOR-INVENTORY.md): every external connection the predecessor system operates today is a **day-1 deliverable** of Territory Grounder — a shipped, loadable module behind a stable `adapters/` interface — never a deferred "reference set". The requirements below enumerate that fleet, one obligation per connector or per tight reference group, across the seven module surfaces: ingest, tracker, notifier+approval, cmdb, actuation, model-provider, and observability. A disabled or unregistered module has no execution path (INV-17); each surface has exactly one implementation per source type driven by configuration (INV-18), so the two LibreNMS deployments and the two Kubernetes clusters are config rows behind one module, not forks.

**Single-org.** This spec is authored single-org: one estate, no `tenant_id`. Per-site variance is configuration behind one implementation. The correlation key is the tracker `issue_id`.

## Requirements

### Ingest surface

- **REQ-801** — [F] event-source receiver · [R] paradigm-rule 3 · [O] INV-04/INV-05/INV-18.
  WHERE the estate is monitored by LibreNMS, the system SHALL provide a loadable ingest module for LibreNMS that authenticates with a configured API token against the configured base URL, normalizes each device-down and device/service/port up-down event to the canonical triage shape, runs in-code dedup, flap, and burst admission before any model dispatch, and drives the NL and GR LibreNMS deployments from two configuration rows behind one implementation.

- **REQ-802** — [F] event-source receiver · [O] INV-04/INV-06.
  WHEN Alertmanager posts a firing or resolved alert, the system SHALL provide a loadable ingest module for Prometheus and Alertmanager that validates each alert against an explicit grammar, normalizes it to the canonical triage shape keyed by alertname and target, correlates a firing-then-resolved transition to one incident, and emits a `triage.requested` event only after in-code dedup and flap suppression. The module SHALL implement the batch ingest extension for Alertmanager's grouped webhook transport with grammar discipline per ALERT: a malformed webhook (or one carrying no alerts) is rejected whole, while a single alert failing the grammar is rejected individually without discarding its well-formed siblings. The target SHALL derive from the instance label when present and SHALL fall back through the Kubernetes workload labels (pod, node, deployment, statefulset, daemonset, job, container) so instance-less kube-state-metrics alerts de-correlate into distinct incidents; the site label and the full label set SHALL propagate into the envelope (the risk classifier and RAG consume them).

- **REQ-803** — [F] event-source receiver · [O] INV-04/INV-20.
  WHERE CrowdSec is a configured security-signal source, the system SHALL provide a loadable ingest module for CrowdSec that ingests scenario and decision events over the configured LAPI endpoint, normalizes them to the canonical triage shape, and routes them through the same in-code dedup, flap, and burst admission path as every other ingest source before any model dispatch.

### Tracker surface

- **REQ-804** — [F] ticketing trigger/sink · [O] INV-05.
  The system SHALL provide a loadable tracker module for YouTrack that opens and reads an issue as the triage entry trigger, uses the issue id as the correlation key across the session lifecycle, transitions issue state through the authenticated REST contract, and posts the terminal audit comment as the session sink.

- **REQ-805** — [R] paradigm-rule 3 · [O] INV-18.
  WHERE an operator selects a non-default issue tracker, the system SHALL provide loadable reference tracker modules for Jira, GitHub Issues, and ServiceNow behind the same tracker interface as YouTrack, each mapping the trigger, correlation-key, state-transition, and audit-sink contract onto its own API, so that a tracker change is a configuration selection and not a code fork.

### Notifier + approval surface

- **REQ-806** — [F] human channel + approval polls · [R] paradigm-rule 2 · [O] INV-12/INV-19.
  The system SHALL provide a loadable notifier-and-approval module for Matrix that renders async out-of-band approval polls onto durable pause/resume state, routes each message to the room resolved from the issue project prefix, authenticates the message sender against the approver set before accepting a vote, binds each vote to the specific pending decision id, and redacts credentials and PII from every posted body.

- **REQ-807** — [F] human channel · [R] paradigm-rule 2.
  WHEN a POLL_PAUSE or AUTO_NOTICE decision requires an out-of-band page, the system SHALL provide a loadable notifier module for Twilio SMS that delivers a page deduplicated by the decision id, carries no command-executing content, and operates as one selectable notifier backend rather than the sole channel.

- **REQ-808** — [F] human channel + approval polls · [O] INV-12.
  The system SHALL provide a loadable notifier-and-approval module for Mattermost that posts notices and approval prompts to a configured channel, authenticates the responding user against the approver set, and binds each response to the specific pending decision id it answers.

- **REQ-809** — [R] paradigm-rule 3 · [O] INV-18.
  WHERE an operator selects a non-default human channel, the system SHALL provide loadable reference notifier modules for Slack, Microsoft Teams, and SMTP email behind the same notifier-and-approval interface, each preserving sender authentication, decision-id binding, and credential and PII redaction as channel-agnostic obligations.

### CMDB surface

- **REQ-810** — [F] CMDB actuation · [O] INV-05.
  The system SHALL provide a loadable CMDB module for NetBox that resolves devices, virtual machines, IP addresses, VLANs, and interfaces by id, serves as the authoritative entity-resolution source that every ingested payload is re-read against before dispatch, and exposes changelog history to the triage context.

### Actuation surface

- **REQ-811** — [F] actuation surface · [O] INV-02/INV-07/INV-13/INV-21.
  The system SHALL provide a loadable actuation module for SSH that executes fixed argv arrays or validated-JSON stdin against a configured host under a per-agent scoped identity, traverses the single pre-execution guard chokepoint before every command, records one `execution_log` row per mutating command with its rollback command bound to the ActionManifest action id, and SHALL NOT expose an interactive shell or a host-key-verification-bypass option.

- **REQ-812** — [F] actuation surface · [O] INV-09/INV-21.
  The system SHALL provide a loadable actuation module for Kubernetes that performs get, describe, apply, patch, rollout, scale, and helm operations against a configured cluster context through typed individually-permissioned operations, traverses the single pre-execution guard chokepoint, and clamps delete and drain to the non-configurable never-auto floor.

- **REQ-813** — [F] actuation surface · [O] INV-09/INV-21.
  The system SHALL provide a loadable actuation module for Proxmox that lists nodes, VMs, and LXC and reads guest configuration and status over an API token, gates every lifecycle operation (start, stop, reboot, shutdown) behind an explicit enable flag and the single pre-execution guard chokepoint, and clamps reboot and halt to the non-configurable never-auto floor.

- **REQ-814** — [F] actuation surface · [O] INV-08/INV-17/INV-21.
  The system SHALL provide a loadable actuation surface for MCP tool modules that registers each tool as a typed capability-scoped adapter reachable only through the single `Execute` chokepoint, withholds every lifecycle-mutating tool behind an explicit enable flag, and grants an unregistered MCP tool no execution path.

### Model-provider surface

- **REQ-815** — [F] centralized model routing · [R] paradigm-rule 3/6 · [O] INV-08.
  The system SHALL provide a loadable model-provider module built on a bundled LiteLLM gateway that exposes one OpenAI-compatible endpoint fronting the configured provider backends z.ai, DeepSeek, Mistral, Ollama, Anthropic, and OpenAI, resolves component-to-provider/model routing through one source of truth, applies the configured auto-fallback ladder on provider error or rate-limit, records real-token usage per request with no fabrication, and treats every model response as untrusted typed data that never becomes control flow, a command string, or a query fragment.

### Observability surface

- **REQ-816** — [F] RAG observability + dead-man switches · [O] INV-15.
  The system SHALL provide a loadable observability module for Prometheus that exposes control-plane and per-connector metrics for scrape, stamps each exported series with a freshness timestamp, and emits `absent()`-guarded staleness metrics so that a dead writer pages rather than reads as healthy.

- **REQ-817** — [F] observability.
  The system SHALL provide a loadable observability module for Grafana that provisions the control-plane dashboards from version-controlled definitions against a configured Grafana instance and rejects hand-edited panels as drift.

- **REQ-818** — [F] observability event stream · [O] INV-14.
  The system SHALL provide a loadable observability module for OpenObserve that exports OTLP traces and logs to a configured endpoint with tracing default-on, so that the trajectory of every session is reconstructable.

- **REQ-819** — [F] observability event stream.
  The system SHALL provide a loadable observability module for Langfuse that records per-session LLM and agent traces to a configured Langfuse endpoint keyed by the session id for post-hoc inspection.

- **REQ-820** — [F] self-monitoring dead-man · [R] paradigm-rule 4.
  The system SHALL provide a loadable observability module for Healthchecks.io that pings a configured dead-man check on each scheduled control-plane heartbeat, so that a missed heartbeat raises an external alert independent of the system's own alert path.

- **REQ-823** — [F] cisco-asa-specialist / triage-researcher syslog reads · [O] INV-02/INV-08/INV-13/INV-17.
  WHERE the estate's devices log to per-site syslog-ng servers, the system SHALL provide a loadable read-only observability connector that exposes two investigation tools to the triage agent — `get-host-logs`, which returns the last N lines of a named device host's log for a date, and `search-host-logs`, which returns fixed-string matches from that log bounded by a maximum match count. The connector SHALL route a device host to its site's syslog-ng server by a configuration-declared site-code prefix and SHALL refuse a host that matches no configured server. It SHALL validate every host against a strict allowlist that rejects a path separator, a parent-directory reference, and a leading dash, and SHALL construct the log path from validated components only. It SHALL execute each read as a fixed argument vector over a non-interactive SSH session opened by an in-process SSH client, SHALL NOT execute a local helper binary for the transport, SHALL verify the server host key against the operator-declared known-hosts file and refuse an unknown or changed host key with no configuration that skips that verification, SHALL refuse every read while no known-hosts file is declared, SHALL NOT construct a command by string concatenation, and SHALL NOT spawn a shell. It SHALL bound every response at the server by a line count and a match count and again in process by a line count and a byte count under an enforced context timeout, SHALL keep the SSH key as a secret reference resolved at read time and parsed in memory without writing key material to a filesystem path, SHALL name only the reference in a key-failure error, and SHALL treat the returned log text as an untrusted observation that never becomes control flow.

### Module framework

- **REQ-821** — [F] the module system · [R] paradigm-rule 3 · [O] INV-17/INV-18.
  The system SHALL provide a capability-scoped module registry behind which every connector is a loadable module bound to one of the seven surfaces, SHALL grant no execution path to a module that is unregistered or disabled (INV-17), and SHALL admit exactly one registered implementation per surface and source type (INV-18), so per-site variance is configuration behind one implementation rather than a second registration.

- **REQ-822** — [F] actuation surface · [O] INV-02/INV-07/INV-09/INV-17/INV-21.
  The SSH actuation module SHALL provide a mutating execution path that is structurally unreachable WHILE the process mutation gate is off: it SHALL report read-only WHILE the gate is off, and every mutating entry SHALL refuse before resolving or running any command WHILE the gate is off. WHERE the gate is enabled, the module SHALL resolve only a capability-declared reversible op_class — `restart-service` (`systemctl restart <unit>`), `reload-service` (`systemctl reload <unit>`), and `restart-container` (`docker restart <container>`) — through a registry allowlist; it SHALL refuse an op_class on the non-configurable never-auto floor even when that class was allowlisted, SHALL refuse a stateful-workload target (keyed on the unit OR container name), SHALL refuse a systemd unit that is not on the operator-declared allowed-units allowlist or a docker container that is not on the operator-declared allowed-containers allowlist, SHALL refuse a unit or container name whose token carries a space, a newline, a slash, or a shell metacharacter, and SHALL record one `execution_log` row whose rollback command is bound to the ActionManifest action id. The module SHALL run the fixed argv over the host-key-verified, non-interactive ssh invocation with each remote argument POSIX-quoted, and SHALL NOT build a command by string concatenation or spawn a shell.

## Persistence & interface contract

Every connector is a loadable module behind a stable `adapters/` interface with a signed capability scope; enablement is a configuration act with no execution path for an unregistered module (INV-17). Ingest modules publish a `triage.requested` event after in-code admission; the tracker module owns the `issue_id` correlation key; actuation modules are reachable only through the single `Execute(ctx, ActionManifest)` chokepoint (INV-21) and append one `execution_log` row per mutating command (INV-07). Model usage is written to the real-token `llm_usage` ledger with no fabrication. See [`docs/ARCHITECTURE.md`](../../docs/ARCHITECTURE.md) and ADR-0005.
