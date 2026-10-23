<!-- spec/008 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/008 — Design: the day-1 connector fleet

How the requirements in `requirements.md` are realized on the Go / Temporal / PostgreSQL / React stack. Where this design and the code disagree, the code is the bug and this document is the intent. This spec is authored **single-org** (one estate, no `tenant_id`); per-site variance is configuration behind one implementation (INV-18).

## The two-layer module system

The repo separates **interfaces** from **implementations**:

- `adapters/<surface>/` — the stable Go interface for each of the seven module surfaces (`ingest`, `tracker`, `notifier`, `cmdb`, `actuation`, `model`, `observability`). An interface names the exact methods a backend must satisfy and nothing about a specific vendor.
- `modules/<surface>/<connector>/` — one loadable implementation per connector, owned by exactly one task in `tasks.json`. Each of the 20 day-1 connectors and 6 reference connectors is a package under this tree.

**Mechanism (ADR-0005, proposed): out-of-process governed plugins.** Each module runs as a separate process/container over a stable protocol (gRPC / go-plugin; **MCP** for tool/actuation modules), signed and capability-scoped. This gives runtime load/unload, third-party modules, and process isolation. INV-17 is the load-bearing property: a disabled or unregistered module has **no execution path** — a capability exists only if its adapter is registered, and a manifest reconciler refuses to boot if the live registered set diverges from the signed manifest. This is the product-grade closure of the predecessor's "dead OpenClaw path still executable" failure class. The registry exposes two views: the sorted-keys `Manifest` (the reconciler input) and a status-aware `Capabilities` list (surface, source, capability slug, enablement) that the read-only API surfaces to the console/ops — so a declared-but-DISABLED member (the Phase-0/1 actuation family, no execution path) is visibly distinct from a live one and can never be mistaken for available.

**Day-1 vs reference is enablement, not existence.** The directive is realized structurally: the 20 predecessor connectors ship registered and enabled in the default single-node `docker-compose` profile; the 6 reference connectors (Jira, GitHub Issues, ServiceNow, Slack, Teams, email) ship built behind the same interface but disabled by default. No connector is a stub or a "someday" example — each is a compiled module a task made pass its acceptance oracle.

## Per-surface realization

**Ingest (REQ-801..803).** Each ingest module is a thin normalizer: authenticate → validate the raw payload against an explicit grammar (INV-04) → map to the one canonical triage shape → run the shared in-code dedup/flap/burst admission before any model spend → re-read the canonical entity from its system-of-record by id (INV-05) → publish a `triage.requested` Temporal signal. INV-18 forces exactly one implementation per source type: `modules/ingest/librenms` serves both the NL and GR deployments as two config rows, not two workflows, and a CI parity test enforces it. The predecessor's ~14 near-identical n8n receivers collapse to this one config-driven set. The LibreNMS module also exposes a topology `EstateSource` (`estate.EdgeSource`) that reads each device's `dependency_parent_hostname` and emits `depends_on` edges (a device depends on its upstream reachability parent) — the authoritative operator-maintained dependency the module previously ignored (PORT-FIDELITY-AUDIT P0-1's LibreNMS arm). It polls each configured deployment read-only, per-source-isolated, and is seeded into the worker only when `TG_LIBRENMS_DEPLOYMENTS` (a `site|url|tokenref` list) is declared (config-not-code).

**Tracker (REQ-804..805).** The `adapters/tracker` interface exposes `Open`/`Read`/`TransitionState`/`Comment` and the `issue_id` correlation key. YouTrack is the day-1 backend; Jira, GitHub Issues, and ServiceNow map the same four-verb contract onto their own APIs. A tracker change is a config selection; the session lifecycle never learns which backend it is.

**Notifier + approval (REQ-806..809).** The `adapters/notifier` interface renders a decision as a message and resolves a returned vote against a pending `decision_id`. The resume primitive is a **Temporal signal** (`SignalWithStart` to the owning workflow), so an approval is bound to exactly the decision it answers with no global cursor and no cross-room misattribution (INV-12). Sender authentication against the approver set and credential/PII redaction are interface obligations every backend inherits — Matrix, Twilio SMS, and Mattermost day-1; Slack, Teams, and email as reference. The Matrix and Mattermost surfaces are also first-class `frontend/` approval-console views (the React console consumes the generated OpenAPI, not a second contract).

**CMDB (REQ-810).** NetBox is the authoritative entity-resolution source. Its module resolves devices/VMs/IPs/VLANs/interfaces by id and is the target of the INV-05 re-read that every ingest payload passes through before dispatch — the payload is a claim, the CMDB record is the fact. The module also exposes a topology `EstateSource` (`estate.EdgeSource`) that reads VM placement and emits `runs_on` edges (a VM depends on its hypervisor host), seeding the causal estate graph the prediction gate reasons over — closing PORT-FIDELITY-AUDIT P0-1's NetBox arm (the module previously contributed no edges, so every blast radius was vacuous). The reader is read-only, paginated, per-source-isolated (a fetch error is surfaced, never a silent empty topology), and wired into the worker only when `TG_NETBOX_URL` is declared (config-not-code). A distinct read-only PVE topology source (`modules/cmdb/pve`) reads guest placement from the cluster resources API and emits `runs_on` edges (an LXC/VM depends on its hypervisor node) at the 0.95 source-of-truth confidence — separate from the proxmox ACTUATION module (which drives reboots and ships OFF) — wired when `TG_PVE_URL` is declared. Together with the LibreNMS dependency reader these three sources seed a multi-source, MAX-ratcheted causal graph.

**Actuation (REQ-811..814).** Every actuation module is reachable only through the single `Execute(ctx, ActionManifest)` chokepoint (INV-21), which traverses the deterministic pre-execution guard (territory / egress / plan-adherence) before any effect. No module can express a shell: SSH runs a fixed argv array (with `StrictHostKeyChecking=no` inexpressible, INV-13) or validated-JSON stdin — and because the remote sshd runs the command through the login shell, each remote argument is POSIX-shell-QUOTED before transport so a space or metacharacter (`;`, `|`, `$(...)`) in an argument can never inject or word-split on the far host (INV-02, the predecessor's per-argument shell-quoting); Kubernetes and Proxmox expose typed individually-permissioned operations. Proxmox lifecycle is DEFAULT-DENY: only recognized ops are expressible, and an unknown op (including a vmid/status path form) is refused. Irreversible op-classes — kubectl delete/drain, a helm teardown (uninstall/delete/rollback) or `kubectl apply --prune` (both delete-equivalent), and Proxmox reboot/reset/shutdown/halt/destroy (a hard power-cycle or permanent deletion, the predecessor's `qm reset`/`qm destroy` floor) — are clamped to the non-configurable never-auto floor (INV-09) regardless of confidence or policy. The reversible lifecycle operations sit behind an explicit enable flag. The MCP surface registers each tool as a capability-scoped adapter; an unregistered tool has no execution path (INV-17).

**SSH mutating path + execution_log (REQ-822).** The SSH module's Phase-2 mutating path (`modules/actuation/ssh/mutate.go`, #21) is structurally unreachable while the process mutation gate is off: `ReadOnly()` is gate-aware and every mutating entry refuses before resolving. WHERE the gate is on it resolves ONLY the capability-declared reversible `restart-service` op_class — the fixed argv `systemctl restart <unit>` — through the never-auto floor, a stateful-workload denylist, and the operator-declared allowed-units allowlist, and it records one `execution_log` row whose rollback is bound to the `action_id` (INV-07). The recording is invoked THROUGH the single chokepoint: after the interceptor's `Do` executes an admitted argv, it calls the module's `ExecLog(action_id, argv)` recorder hook — which re-derives the compensating inverse via the SAME `resolveOp`/`RecordExec` construction — and `Do` durably records the forward+inverse to the tamper-evident ledger. While mutation is off nothing executes, so nothing is recorded. The module never builds a command by string concatenation and never spawns a shell.

**Rollback-side classification (REQ-824, TG-464).** `classifyArgv` is SIDE-AWARE: each templated class contributes its forward shape and — when it declares a distinct non-empty `rollback_template` — its compensating shape, matched by the same `opschema.MatchTemplate` primitive over a synthetic single-template spec (no second matcher). `guardMutatingArgv` compares against the canonical argv of the MATCHED side; `resolveOp` already returns both (`spec.Argv` + `spec.RollbackArgv` — the same rendering the runner's rollback lane seals), so the sides cannot drift. The ambiguity refusal spans the whole forward∪rollback union, and a `rollback_template` element-equal to the forward is treated as the one shape it is rather than a self-ambiguating twin. `ExecLog` on a rollback-side argv records the executed inverse as the command and the class's forward argv as its bound rollback (undoing an undo re-runs the forward — the proxmox leaf's start↔stop derivation). This is what arms TG-462's manual-rollback lane at the leaf: the sealed, approved inverse (`systemctl stop <unit>`, `docker stop <container>`) now has the execution path the host guard's generated allowlist already granted it.

**SSH CA/signed-cert actuation credentials (REQ-822, TG-423).** The native actuation runner (`modules/actuation/ssh/native.go`) optionally presents a short-lived OpenBao-signed USER CERTIFICATE instead of the static private key. When the composition root supplies a cert-signer hook (armed by `TG_SSHCA_ADDR`, read via `planeEnv` so it is EMPTY on the triage plane — the ssh-CA token is never acquired there, the same plane-isolation the bare key relies on), each Run mints a fresh certificate for the actuation key's public key from OpenBao's `ssh` secret engine (CA mode) and presents it (`ssh.NewCertSigner`), so the target trusts it via sshd's `TrustedUserCAKeys` and the exposure window becomes the cert TTL rather than "until a human rotates the key". A per-Run signing failure fails the actuation CLOSED — never a silent fall back to the long-lived bare key — and an un-armed deployment (`TG_SSHCA_ADDR` unset) takes the bare-key path byte-for-byte. The concrete signer lives in `core/credential/sshca` and is injected (core must not import modules); the cert TTL and allowed principals are policy on the OpenBao role, not in the worker. This is the SSH slice of dynamic target credentials (TG-320); the sibling Postgres `database`-engine slice is TG-422's `dyn:` scheme. Arming is a two-part operator act: provision the engine AND roll the CA public key into every actuation target's `TrustedUserCAKeys`.

**Verified remote-kill on cancellation (TG-80 P1-4, 2026-08-22).** The native actuation runner's context watchdog now covers the DIAL/HANDSHAKE phase only; once a session is running, a cancellation goes through signal escalation over the still-open channel — `SIGTERM`, a bounded grace (`killGrace`, 5s), `SIGKILL`, a second grace — and only then do the deferred closes drop the session and transport. Before this the watchdog closed the TCP link the instant the context ended and the remote command ran on, orphaned on the target. The runner returns the typed `ErrRemoteCancelled` wrapping the context error and naming which signal ended the run (or that `SIGKILL` went unacknowledged before the close), so the interceptor records a CANCELLED terminal rather than a generic execute failure. The oracle is an in-process `x/crypto` sshd whose exec'd command ends only on a signal: it must receive `TERM`, and a `TERM`-ignoring command must be escalated to `KILL` inside the grace windows.

**syslog-ng investigation reads (REQ-823).** The read-only syslog-ng connector (`modules/observability/syslogng`) reaches each per-site syslog server through a NATIVE in-process SSH client (`golang.org/x/crypto/ssh`), never a local `ssh` binary — the worker ships on a distroless static image that carries no shell and no client executable, so an exec-based transport would fail at first use in production. The runner resolves the server row's key REFERENCE at read time and parses it in memory (`ssh.ParsePrivateKey`, INV-13): key material never touches a filesystem path, and a key failure names the reference only. Host-key verification is mandatory and fails closed: the operator declares ONE OpenSSH known_hosts file covering every configured server (`TG_SYSLOGNG_KNOWN_HOSTS`; known_hosts is a multi-host format, so a per-row file would fragment it for no gain), `x/crypto/ssh/knownhosts` verifies the presented key against it, an unknown or changed key aborts the handshake, and no code path in the package expresses an insecure callback or trust-on-first-use. Auth offers exactly one method (the parsed key), no PTY is requested, and the fixed remote argv is POSIX-quoted per element into ONE command word the remote sshd's login shell re-parses into exactly the fixed vector (INV-02) — no local shell exists anywhere in the path. The tool-level context timeout is enforced end to end: the TCP dial honors the context, and a watchdog closes the transport on expiry, aborting the handshake, the session, and any in-flight read.

**Model-provider (REQ-815).** The `agent/` service is a native Go ReAct/tool-calling loop that calls the bundled **LiteLLM gateway** — one OpenAI-compatible endpoint fronting the six provider backends — never a CLI subprocess. `modules/model/litellm` is the gateway module; `modules/model/<provider>` is each backend behind it. One resolver of record maps component→provider/model, and it is `deploy/litellm-config.yaml`'s `model_list` — NOT a Go struct (TG-298: a Go `adapters/model.Resolver` holding the same table existed for months, was constructed only in this spec's own acceptance test, reached no composition root, and was deleted rather than wired; two tables for one decision is not "one source of truth"). The Go side selects a tier NAME at each call site; the config maps it to a provider. `temporal/runner.TestEverySelectedTierIsServedByTheGatewayConfig` holds the two halves together. The auto-fallback ladder (default z.ai → DeepSeek → Mistral → Ollama, with Anthropic/OpenAI as configurable fallback) is expressed as config. Real-token usage is written to `llm_usage` with no fabrication. Model output enters the system only as untrusted, typed, delimited data (INV-08) — it never becomes control flow, a command string, or a query fragment. DeepSeek is a reasoning model whose response carries `[thinking, text]` blocks; the parser joins `type=='text'`.

**The production model-path breaker (REQ-826/827).** The guard lives on `adapters/model.Gateway`, not on each
caller, and not only in `modules/model/litellm`. The reason is the one already written into the Gateway's
`CallObserver`: every production caller — agent loop, judge cron, skill generator, offline gate, calibrator,
RAG embedder — shares ONE `*Gateway`, so a chokepoint there cannot be bypassed by adding a caller, while a
per-caller decorator can. The alternative (make the guarded module the exercised one) was rejected: its
`Complete(ctx, component, user, msgs) (string, Usage, error)` signature is not the `agent.Completer` shape the
whole tree consumes, so adopting it would rewrite the agent loop, the judge, the skill generator and the
offline runner — and would still leave `adapters/model.Gateway` reachable and unguarded, i.e. a bypass lane.
`adapters/model/breaker.go` holds `Breakers`, a lazy registry of one `core/breaker.Breaker` per model TIER
over the SAME shared store as the mutation breaker (`mutation_breaker_state`, one row per name), so a trip is
cross-process and survives restart. `modules/model/litellm` now names its per-rung breakers through the same
exported `model.BreakerName` slug rule, so a rung and a tier denoting one upstream share one row instead of
half-counting into two. The worker exposes every named row on `/metrics`, which is the "observable" half of
CONSTITUTION.md:130 that the mutation-only exporter did not cover.

Direction of failure is declared per lane and is NOT uniform: the breaker guards the advisory/read lane, so a
LOST BREAKER STORE fails OPEN (the call proceeds, the degradation is logged) — the inverse of spec/013
REQ-1210, where an unreadable actuation breaker reads OPEN and halts. An OPEN CIRCUIT, by contrast, is loud in
both lanes: the agent loop stops and proposes nothing (actuation fails closed), and the judge batch returns a
failed activity with every session left unmarked (judging fails loud). A 400 never trips — that is a defect in
the request this process sent, and tripping on it would let one over-long prompt short-circuit every other
component's model calls.

**Observability (REQ-816..820).** Prometheus exposition stamps every series with a freshness timestamp and emits `absent()`-guarded staleness so a dead writer pages rather than reading as healthy (INV-15). Grafana dashboards are provisioned from version-controlled JSON with hand-edited panels rejected as drift. OpenObserve receives session traces/logs (as JSON via its native `_json` bulk route, not OTLP) with tracing default-on (closing the predecessor's ~0.1%-of-spans gap). Langfuse records per-session LLM/agent traces keyed by session id. Healthchecks.io is the external dead-man: a ping on each control-plane heartbeat raises an alert on the outside, independent of the internal alert path.

## Out of scope

The classifier that decides autonomy band over an actuation is spec/001; the fail-closed prediction gate the actuation traverses is spec/002; the interface-contract generation (OpenAPI/AsyncAPI) is spec/006; the spec↔code lockstep binding governed connector files is spec/007. This spec owns only the loadable connector modules and their interfaces.

## The host guard's allowlist is GENERATED from the op-class registry

`/etc/tg-actuator-guard.allow` holds the exact `SSH_ORIGINAL_COMMAND` strings TG may send; the guard compares
byte-for-byte with `grep -qxF` and exits 42 otherwise. That list was hand-authored in
`deploy/actuation-guard/install.sh`, which emitted three shapes (`systemctl restart|reload <unit>`,
`docker restart <container>`) while the registry could emit five — `systemctl start` (start-service) and
`docker start` (start-container) were absent, and BOTH are graduated to AUTO. TG could therefore choose a
start autonomously, clear all six gates, record the approval in the ledger, and be refused by the host.

The rollback half was worse: `spec.RollbackArgv` renders `systemctl stop` / `docker stop` and no code path
ever wrote either verb into the allowlist, so an action could be taken and never undone.

`tools/guardallow` now renders the allowlist by walking `opschema.Specs()`, emitting BOTH the forward argv and
the rollback argv for each operator-supplied subject, through `ssh.RemoteCommand` — the same function that
renders what TG actually sends, so the quoting cannot diverge. `install.sh` consumes that file and REQUIRES
it (`TG_GUARD_ALLOW_SRC`) rather than defaulting: silently falling back to a hand-written subset is the defect
being removed.

The operator still owns the SUBJECTS — which units and which containers a given host permits. That is what
makes the guard narrower than the registry, and it is the reason the guard exists; what it must not be is a
different SHAPE from the registry.

`RemoteCommand` is exported for this reason. A second implementation of the quoting would produce an
allowlist that looks correct and fails byte-for-byte at the guard.

**The rollback-necessity READ ships with the verbs (TG-464).** Before firing a stop-`*` inverse the actuate
plane confirms the unit is still RUNNING with `systemctl is-active <unit>` over the SAME key this guard pins —
the rollback necessity probe's service lane, the one pre-effect state surface that plane can read (the
LibreNMS alert read is 403-scoped-out there, TG-461). The shape is authored ONCE as
`ssh.ServiceActiveProbeArgv` and consumed by both the probe (a read-only ssh module — no gate, so it cannot
resolve a mutating command) and `tools/guardallow`, which emits the line only for a unit the registry actually
rendered a verb for: a subject the registry refuses gets no read line either. A guard that authorised the stop
but denied the read would leave every service rollback refusing fail-closed at the necessity gate — the same
act-then-cannot-undo asymmetry the generated rollback half exists to prevent, one layer up. The probe treats
only `is-active`'s own completed-read exits (0 active; 3/4 not running) as an answer; the guard's 42 and the
transport's 255 stay "unestablished", so a denial can never masquerade as "nothing to undo".

## Terminology note (2026-08-11, TG-112)

The MCP module's per-module tool-class flag is `WithMutatingTools()` — renamed from `WithMutationEnabled`
under the TG-112 terminology retirement. It is a per-module enable for lifecycle-mutating MCP tools,
unrelated to the retired global mutation switch, and the old name invited exactly that confusion. No
behavior changed; floor rules still apply upstream.

## The SSH leaf proves its transport pre-flight (2026-08-22, TG-81 b4)

`ssh.Module.ProbeReachable` implements the interceptor's gate-4h3 capability: a bounded plain TCP dial
to the leaf's configured sshd endpoint — transport proof only, no authentication, no command, nothing
executed. The refusal detail carries the dial error so an operator reads "host down" where they used to
read "effect leaf exited"; the single-host binding means the probe answers for the one endpoint this
leaf can ever execute on (the host-match gate 4g already refused any target mismatch upstream).
