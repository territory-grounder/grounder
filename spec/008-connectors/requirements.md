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
  WHEN a lifecycle operation reports that the guest is ALREADY in the state the operation was trying to
  reach, the module SHALL re-read the guest's live status and, WHERE the observed state matches the
  operation's goal, SHALL return success rather than a failed mutation. The short-circuit SHALL be keyed on
  the provider's own message TOGETHER WITH the operation (an "already running" response never excuses a
  failed stop) and SHALL NEVER be inferred from a status code alone, and it SHALL be an OBSERVATION — the
  guest's state is re-read, never assumed from the message. *Rationale:* a start issued against a guest that another actor
  has already recovered is the DESIRED outcome, not a failure; recording it as a failed mutation
  inflates the apparent failure rate and denies the heal-success axis an outcome TG effectively achieved.
  Measured live before this requirement: 50 of 72 refusals in one week were this race.
  The module's per-guest allowlist SHALL be resolved AT ACTUATION TIME, not frozen at construction
  (TG-232). A guest the operator adopts SHALL become actuatable without a process restart, and a guest the
  operator revokes SHALL stop being actuatable immediately — live resolution that only widens is worse than
  a frozen list, because the revocation appears to take effect and does not. The default-deny floor is
  unchanged and SHALL survive live resolution: an actuator with no allowlist provider, or one whose
  provider yields nothing usable, SHALL refuse every guest. *Rationale:* the ssh lane already resolved its
  per-target grant live, so an adopted systemd or docker entry took effect at once while an adopted GUEST
  waited for the next worker start — the manifest said "approved", the actuator declined, and nothing
  distinguished that from a default-deny refusal. An operator who cannot tell those two apart learns to
  distrust the refusal, which costs more than the delay.

- **REQ-814** — [F] actuation surface · [O] INV-08/INV-17/INV-21.
  The system SHALL provide a loadable actuation surface for MCP tool modules that registers each tool as a typed capability-scoped adapter reachable only through the single `Execute` chokepoint, withholds every lifecycle-mutating tool behind an explicit enable flag, and grants an unregistered MCP tool no execution path.

### Model-provider surface

- **REQ-815** — [F] centralized model routing · [R] paradigm-rule 3/6 · [O] INV-08.
  The system SHALL provide a loadable model-provider module built on a bundled LiteLLM gateway that exposes one OpenAI-compatible endpoint fronting the configured provider backends z.ai, DeepSeek, Mistral, Ollama, Anthropic, and OpenAI, resolves component-to-provider/model routing through one source of truth, applies the configured auto-fallback ladder on provider error or rate-limit, records real-token usage per request with no fabrication, and treats every model response as untrusted typed data that never becomes control flow, a command string, or a query fragment.

- **REQ-826** — [F] `scripts/lib/circuit_breaker.py` + the four named RAG breakers (IFRNLLEI01PRD-631) ·
  [O] INV-15/INV-22 · docs/CONSTITUTION.md:130 ("named, observable circuit breakers with persisted state").
  Every PRODUCTION model-gateway call — each chat completion and each embedding — SHALL traverse a NAMED
  circuit breaker keyed on the model tier, whose three-state position is held in the shared cross-process
  breaker store and exposed as one `circuit_breaker_state` series per name. Consecutive upstream failures on
  one tier SHALL open that tier's circuit and SHALL NOT open another tier's. An open circuit SHALL
  short-circuit the call with NO round trip until its cooldown elapses, and SHALL then admit exactly one
  half-open probe whose success closes the circuit and whose failure re-opens it. A response carrying HTTP
  400 SHALL NOT accrue toward a trip, and a 200 whose completion text is blank SHALL NOT accrue toward a trip.
  *Rationale:* the breaker machine and this module's per-rung guard both existed, but the module has no
  production constructor — the judge cron, the skill generator and the RAG embedder call
  `adapters/model.Gateway` directly, so a gateway flap degraded them unbounded (PORT-FIDELITY-AUDIT #24,
  TG-221). A guard on the shared Gateway cannot be bypassed by adding a caller; a per-caller decorator can.

- **REQ-827** — [O] INV-08/INV-22 · [R] a degraded model plane fails in a DECLARED direction, never into an
  empty result.
  WHEN a production model-gateway circuit is open, the system SHALL return a typed error carrying a distinct
  breaker-open class and SHALL NOT return an empty completion, an empty embedding, or a score with a nil
  error. The agent loop SHALL stop without producing a proposal, so the actuation-relevant path fails CLOSED.
  The session-judge batch SHALL halt with a failed activity run and SHALL leave every session of that batch
  unmarked and free of judgment rows, so a trip never becomes an empty scorecard nor a session recorded as
  judged that no judge scored. IF the breaker's own state store cannot be read or written, THEN the guard
  SHALL admit the call and SHALL report the degraded state, so the loss of breaker persistence never blocks a
  healthy gateway.
  *Rationale:* this breaker guards the advisory/read lane, so its store-loss direction is the OPPOSITE of the
  actuation breaker's (spec/013 REQ-1210 fails CLOSED on an unreadable store). Both directions are deliberate
  and neither is a default: a safety breaker we cannot observe is treated as tripped; an inference breaker we
  cannot observe is treated as absent.

### Observability surface

- **REQ-816** — [F] RAG observability + dead-man switches · [O] INV-15.
  The system SHALL provide a loadable observability module for Prometheus that exposes control-plane and per-connector metrics for scrape, stamps each exported series with a freshness timestamp, and emits `absent()`-guarded staleness metrics so that a dead writer pages rather than reads as healthy.

- **REQ-817** — [F] observability.
  The system SHALL provide a loadable observability module for Grafana that provisions the control-plane dashboards from version-controlled definitions against a configured Grafana instance and rejects hand-edited panels as drift.

- **REQ-818** — [F] observability event stream · [O] INV-14.
  The system SHALL provide a loadable observability module for OpenObserve that exports session traces and logs to a configured endpoint with tracing default-on, so that the trajectory of every session is reconstructable. (Records ship as JSON to OpenObserve's native `_json` bulk-ingest route, NOT the OTLP `/v1/{traces,logs}` routes — TG's records are session-keyed span strings and stamped samples, not OTLP span/log objects.)

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

- **REQ-824** — [O] INV-02 · [O] INV-07 · [R] the leaf's argv→op-class classification is half of an integrity
  check, not a convenience.
  The SSH actuation module SHALL derive its argv→op-class classification from the op-class registry rather
  than from a list maintained in the module. The match SHALL be STRUCTURAL — element count, then literal
  equality at every non-slot position — and SHALL NOT parse a command string, prefix-match, case-fold, or
  accept an empty slot value. The classifiable set SHALL be the forward∪rollback template UNION (TG-464): for
  a templated class declaring a DISTINCT non-empty `rollback_template`, the rendered compensating argv SHALL
  classify to that class on the ROLLBACK side, through the same structural matcher — never a second matcher —
  and `guardMutatingArgv` SHALL compare the argv against the canonical argv OF THE MATCHED SIDE
  (`spec.Argv` forward, `spec.RollbackArgv` rollback), with the never-auto floor, stateful clamp, token
  validation, and operator allowlists applied to the (class, target) pair identically on both sides. An argv
  claimed by more than one (op-class, side) — two forwards, two rollbacks, or a forward colliding with
  another class's rollback — SHALL be refused entirely rather than resolved to either. An argv the registry
  cannot produce on either side SHALL have NO execution path. The `execution_log` row for an EXECUTED
  rollback-side argv SHALL record the executed inverse as its command and the class's forward argv as its
  bound rollback.
  *Rationale:* `guardMutatingArgv` classifies an argv, re-derives the canonical argv from the class it gets
  back, and refuses on mismatch — so a wrong classification is not caught by the round-trip, it is *hidden*
  by it. With two claimants the check passes while the action is recorded, allowlisted and graduated as a verb
  it is not: a same-shape, different-governance substitution. The module previously held a linear if-chain
  naming four verbs beside a registry that builds the same four shapes; a verb absent from the chain was
  silently unexecutable here while appearing fully registered everywhere else. The rollback side repeated the
  same shape one layer up (TG-462/TG-464): the manual-rollback lane sealed and approved a compensating
  `systemctl stop`/`docker stop` that this leaf then refused as unclassifiable — the host guard's allowlist
  already carried both sides (guardallow walks the same registry), so the leaf was the one place the inverse
  had no execution path and "zero inverses have ever run". See spec/013 REQ-1226 for the registry-side half
  of this property.

- **REQ-825** — [O] INV-02 · [O] INV-07 · [O] INV-22 · [R] a verb the registry declares and the leaf refuses is
  not a verb, it is a rumour.
  The SSH actuation module's set of resolvable op-classes SHALL be DERIVED from the op-class registry, not held
  as a list in the module. Its runtime controls SHALL be keyed on the class's declared FAMILY — a
  service-lifecycle slot validated as a systemd unit token and gated by the operator allowed-units allowlist, a
  container-lifecycle slot validated as a container name and gated by the operator allowed-containers
  allowlist. A class whose family this module has NO vocabulary for SHALL be refused OUTRIGHT as having no
  execution path, and SHALL NOT fall through to another family's validation or allowlist. Every registered
  ssh-argv class in a known family SHALL resolve to a non-empty argv AND a non-empty bound rollback; a class
  that does not SHALL be a finding, not a silent refusal.
  *Rationale:* measured the day this shipped. `start-container` was added as a pure registry block — no
  builder, no case in the leaf — and it registered, validated, classified, round-tripped and passed the entire
  test suite while refusing at the leaf with *"op_class has no reversible execution path (not allowlisted)"*.
  Nothing failed. The verb was registered, prompt-rendered, graduation-tracked and INERT, which is the same
  shape as `AutoEligible` shipping unreachable and the tier floor shipping decorative. Deriving the set from
  the registry is not a widening of authority: a class still traverses the never-auto floor, the
  stateful-workload clamp, token validation, the operator allowlist (an EMPTY allowlist refuses every target),
  the mode chokepoint and the target host's own argv allowlist. What it stops doing is refusing for the one
  reason that carried no information — that a second list had not been updated. The fall-through clause is the
  sharp one: a family the leaf does not understand is one param-name away from being validated as a systemd
  unit and gated by the WRONG allowlist, and the refusal would read as "bad target" rather than "unsupported
  class".

## Persistence & interface contract

Every connector is a loadable module behind a stable `adapters/` interface with a signed capability scope; enablement is a configuration act with no execution path for an unregistered module (INV-17). Ingest modules publish a `triage.requested` event after in-code admission; the tracker module owns the `issue_id` correlation key; actuation modules are reachable only through the single `Execute(ctx, ActionManifest)` chokepoint (INV-21) and append one `execution_log` row per mutating command (INV-07). Model usage is written to the real-token `llm_usage` ledger with no fabrication. See [`docs/ARCHITECTURE.md`](../../docs/ARCHITECTURE.md) and ADR-0005.

- **REQ-828** — [O] INV-13 · host-key verification SHALL verify the key that was actually pinned.
  Every SSH client the system opens — the actuation lane, the syslog-ng read tools, and their probes —
  SHALL advertise, as its offered host-key algorithms, EXACTLY the algorithms the operator-declared
  known-hosts file holds for that destination, and SHALL NOT leave the offered set to the SSH library's
  default preference order. WHERE the destination is absent from the known-hosts file, the client SHALL
  advertise nothing and be refused by the verification callback with an unknown-host error. The callback
  and the advertised algorithm list SHALL be set together through one shared helper; neither SHALL be
  settable alone.
  *Rationale:* the two are a pair, and setting only the callback verifies a key the server was never going
  to present. On 2026-08-02 both site syslog servers reported a host-key MISMATCH — the alarm that means a
  machine may be impersonated — and neither key had changed. `golang.org/x/crypto/ssh` orders ECDSA and RSA
  ahead of Ed25519 by default, so against a stock OpenSSH server offering
  `rsa-sha2-512, rsa-sha2-256, ecdsa-sha2-nistp256, ssh-ed25519` the client negotiated ECDSA while
  known_hosts pinned only `ssh-ed25519`; the callback found the host, found no ECDSA key for it, and
  returned the changed-key error. The cost is asymmetric and paid twice: the honest response to a
  host-key mismatch is expensive and alarming, and in the meantime the failure is SILENT — the syslog tools
  stayed registered and routed and failed at read time as what looked like a transient network fault, so
  both sites ran with no device logs during triage. On the actuation lane the same defect refuses a heal
  while reporting an impersonation alarm. This requirement is enforced structurally: no file outside the
  shared helper may name the callback field.

- **REQ-829** — [O] INV-02 · [O] INV-09 · [R] ADR-0012 · the Cisco interactive-SSH transport is READ-ONLY by
  construction (TG-85 slice 1). IOS/ASA expose no argv-exec channel — a command runs inside an interactive
  PTY CLI — so a distinct transport (`modules/actuation/cisco`) opens a crypto/ssh PTY and drives a
  prompt-anchored send/expect exchange, dialing through the SAME fail-closed recipe as the native-ssh leaf
  (host-key verified via the shared helper of REQ-828, credential a secret-ref resolved in memory, a context
  watchdog bounding the whole exchange). It SHALL admit ONLY read-only diagnostic commands: the first token
  MUST be a member of a closed read-verb allowlist (`show`, `ping`, `traceroute`, `packet-tracer`), NO token
  anywhere may be a mutating/mode-changing word (`configure`, `write`, `copy`, `reload`, `erase`, `clear`,
  `no`, `enable`, `debug`, `terminal`, …), and NO token may contain a CLI separator or redirection character
  (`| ; & < > $` backtick newline) — because `show <x> | redirect flash:<file>` WRITES a file and
  `show run | redirect tftp://<host>` EXFILTRATES the running-config, a write reached through a `show` verb.
  The `redirect`/`tee`/`append` verbs are ALSO on the forbidden set as defense in depth behind the separator
  refusal. A refused command SHALL NOT dial the device. The transport's `ReadOnly()` SHALL be unconditionally
  true (no constructor makes it mutating), and it holds no mutation gate and no op-class registry; the only
  command the transport itself originates is the fixed pager-off (`terminal length 0`), never a model input,
  and `terminal` is independently on the forbidden set so a model command can never impersonate it. Even
  though the wire is a PTY, the model can express nothing but a read through this leaf, so the connector does
  not expose an interactive shell (REQ-811). Enable-mode, any `configure`/write path, jump-host chaining, and
  per-target routing are out of scope for this slice and gated at never-auto (ADR-0012) until separately
  designed and owner-armed.
  *Rationale:* a Cisco surface is permitted only vendor-official + read-only behind the interceptor; the whole
  risk of a PTY transport is that an interactive shell can run anything, so the safety of this slice rests
  entirely on the command-admission guard, which is therefore a spec-governed safety-critical control — a
  future edit that weakens it must trip the spec-diff gate, not slip through as an ordinary refactor.
