# Connector Inventory — the day-1 external-connection fleet

**Status:** founding document · **Provenance tags:** `[F]` foundation (predecessor design TG inherits) · `[R]` product reframe · `[O]` audit overlay. Source ids appear inline — `spec/008`, `INV-NN`, `paradigm-rule N`.

Siblings: **PRODUCT.md** §6 (the module system), **CAPABILITY-INVENTORY.md** Area 5 (integrations), **ARCHITECTURE.md** (the interface catalog + ADR-0005), and the executable spec **[spec/008-connectors](../spec/008-connectors/)** (REQ-801..820).

---

## Operator directive — the whole point of this document

> **Every external connection the predecessor system actually operates is a DAY-1 deliverable of Territory Grounder — a first-class, shipped, loadable module — not a "reference set" example to be built later.**

The predecessor is a battle-tested, single-estate governed-autonomy platform. It talks to a concrete fleet of external systems every day: LibreNMS at two sites, Prometheus/Alertmanager, CrowdSec, YouTrack, Matrix, Twilio SMS, Mattermost, NetBox, SSH hosts, two Kubernetes clusters, Proxmox, a bank of MCP tool servers, a LiteLLM model-gateway fronting six providers, and five observability sinks. TG does **not** get to ship with those demoted to "someday" examples. They land in the box, enabled, on day one. This inventory is the bill-of-materials that binds that directive to a shipped module per row.

Connectors the predecessor does **not** operate today (Jira, GitHub Issues, ServiceNow, Slack, Microsoft Teams, SMTP email) ship as **reference** adapters behind the same interfaces so an adopter on a different stack is not forked — they are genuinely a reference set, and the `day-1 tier` column below marks the boundary explicitly.

**Scope note (single-org).** This inventory and its owning spec are authored **single-org**: one estate, no `tenant_id`. Per-site variance (the NL and GR LibreNMS deployments, the two Kubernetes clusters) is **configuration behind one implementation** (INV-18: exactly one implementation per source type, driven by config — two config rows, never two workflows), which is orthogonal to tenancy.

**Governed-by-construction (applies to every row).** Each connector is a loadable module behind a stable `adapters/` interface; a disabled or unregistered module has **no execution path** (INV-17). Actuation modules traverse the single `Execute(ctx, ActionManifest)` guard chokepoint (INV-21) and can express no shell (INV-02/INV-13). Model output is untrusted typed data (INV-08). Ingest payloads are grammar-validated and the canonical entity is re-read by id before dispatch (INV-04/INV-05).

---

## How to read the tables

- **connector** — the concrete external system, one shipped module (or one config instance of a module).
- **module surface** — one of the seven loadable surfaces: `ingest` / `tracker` / `notifier+approval` / `cmdb` / `actuation` / `model-provider` / `observability`.
- **predecessor integration** — how the predecessor wires it today (the day-1 provenance), or `none` for a reference adapter.
- **day-1 tier** — `day-1` (shipped enabled, honoring the directive) or `reference` (shipped behind the interface, disabled by default, for stack portability).
- **notes** — the reframe / interface / safety binding for TG.

---

## Ingest — alert & signal sources (REQ-801..803)

Thin per-source adapters normalizing to one canonical triage shape, running dedup/flap/burst in code before any model spend, each publishing a `triage.requested` event (CAPABILITY-INVENTORY Area 5; INV-04/INV-05/INV-18).

| connector | module surface | predecessor integration | day-1 tier | notes |
|---|---|---|---|---|
| LibreNMS (NL) | ingest | n8n LibreNMS receiver `Ids38SbH48q4JdLN`; API token vs `dc1nms01.example.net` (self-signed) | day-1 | Device/service/port up-down + device-down normalized to canonical shape. |
| LibreNMS (GR) | ingest | n8n LibreNMS receiver `HI9UkcxNDxx6MEFD`; dedicated `dc2nms01.example.net` instance | day-1 | **Same `modules/ingest/librenms` implementation** as NL, second config row (INV-18) — not a forked workflow. |
| Prometheus / Alertmanager | ingest | n8n Prometheus receivers `CqrN7hNiJsATcJGE` (NL) / `bdAYIiLh5vVyMDW7` (GR); `alertmanager-twilio-bridge` | day-1 | Validates against an explicit grammar; correlates firing→resolved transitions before emitting `triage.requested`. |
| CrowdSec | ingest | n8n CrowdSec receivers `eJ0rX9um4jBuKBtn` (NL) / `dr37fPJAZ9a3JRdT` (GR) | day-1 | Scenario/decision events over the configured LAPI; same in-code admission path as every other source. |

## Tracker — issue trigger & terminal audit sink (REQ-804..805)

The tracker is both the entry trigger and the terminal audit sink; its issue id is the correlation key across every subsystem (single-org: `issue_id`, not a tenant tuple).

| connector | module surface | predecessor integration | day-1 tier | notes |
|---|---|---|---|---|
| YouTrack | tracker | Webhook state→In Progress trigger; completion-comment sink; `youtrack` MCP; REST state transitions | day-1 | The primary tracker; establishes the trigger/correlation/state/sink contract the interface exposes. |
| Jira | tracker | none | reference | Maps the trigger/correlation/state/sink contract onto Jira's API behind the tracker interface. |
| GitHub Issues | tracker | none | reference | Same contract; issue+label model. |
| ServiceNow | tracker | none | reference | Same contract; incident-table model. |

## Notifier + approval — the human circuit-breaker channel (REQ-806..809)

Async out-of-band approval polls on durable pause/resume state (Temporal signals as the resume primitive), sender-authenticated against the approver set, with credential/PII redaction as a channel-agnostic obligation (INV-12 least-privilege; INV-19 vote ledger).

| connector | module surface | predecessor integration | day-1 tier | notes |
|---|---|---|---|---|
| Matrix | notifier+approval | `@claude` bot on `matrix.example.net`; Bridge workflow `QGKnHGkw4casiWIU`; room routing by project prefix | day-1 | Primary HITL channel; also a first-class `frontend/` approval console surface. |
| Twilio SMS | notifier+approval | `alertmanager-twilio-bridge.py` `/alert-session`; tier-1 dead-man SMS | day-1 | One selectable page channel (not the sole channel); deduplicated by decision id; carries no command-executing content. |
| Mattermost | notifier+approval | `mattermost.example.net` | day-1 | Notices + approval prompts to a configured channel; response bound to the pending decision id. |
| Slack | notifier+approval | none | reference | Same notifier+approval interface; sender-auth + decision-id binding + redaction preserved. |
| Microsoft Teams | notifier+approval | none | reference | Same interface. |
| SMTP email | notifier+approval | provenance-only: SMTP scanner-report delivery (`weekly-scan.sh` → `mxmx.email`) | reference | Promoted from report-only sender to a full notifier reference adapter behind the interface. |

## CMDB — authoritative entity resolution (REQ-810)

| connector | module surface | predecessor integration | day-1 tier | notes |
|---|---|---|---|---|
| NetBox | cmdb | `netbox` MCP over the NetBox CMDB (devices/VMs/IPs/VLANs/interfaces/changelogs) | day-1 | The authoritative source every ingested payload is re-read against before dispatch (INV-05); preferred over LibreNMS for device identity. |

## Actuation — the effect channel (REQ-811..814)

Typed, individually-permissioned adapters reachable only through the single `Execute(ctx, ActionManifest)` chokepoint (INV-21); no shell anywhere (INV-02/INV-13); lifecycle-mutating operations enable-flag-gated; irreversible classes clamped to the never-auto floor (INV-09).

| connector | module surface | predecessor integration | day-1 tier | notes |
|---|---|---|---|---|
| SSH | actuation | n8n SSH Execute nodes as `claude-runner`; `one_key` identity; `execution_log` undo-stack | day-1 | Fixed argv / validated-JSON stdin only; per-agent scoped identity; no interactive shell, no host-key-bypass option expressible. |
| Kubernetes (kubectl/helm) | actuation | `kubernetes` MCP against the NL + GR clusters | day-1 | get/describe/apply/patch/rollout/scale + helm; delete/drain clamped to the never-auto floor. |
| Proxmox | actuation | `proxmox` MCP (API tokens); lifecycle gated by `PVE_ALLOW_LIFECYCLE` | day-1 | Read ops open; start/stop/reboot/shutdown behind an explicit enable flag; reboot/halt clamped to the never-auto floor. |
| MCP tool modules | actuation | `netbox` · `n8n-mcp` · `gitlab-mcp` · `youtrack` · `proxmox` · `codegraph` · `opentofu` · `tfmcp` · `kubernetes` | day-1 | The typed MCP tool surface; each tool a capability-scoped adapter; an unregistered tool has no execution path (INV-17). |

## Model-provider — the LiteLLM gateway + provider backends (REQ-815)

One component→provider/model resolver of record; the bundled LiteLLM gateway exposes one OpenAI-compatible endpoint fronting N providers with an auto-fallback ladder as config; real-token usage only, no fabrication; every response is untrusted typed data (INV-08).

| connector | module surface | predecessor integration | day-1 tier | notes |
|---|---|---|---|---|
| LiteLLM gateway | model-provider | shared `dc1litellm01:4000` (v1.85.0); `scripts/lib/model_routing.py` resolver | day-1 | The bundled model-gateway module; one endpoint, N backends, ladder + retries + rate-limit handling. |
| z.ai | model-provider | live Claude-Code-plane default (`glm-5.2`/`glm-4.7`) via `claude-provider.sh` | day-1 | Default primary rung of the fallback ladder. |
| DeepSeek | model-provider | eval plane via LiteLLM (`deepseek-v4-pro`, reasoning model) | day-1 | Second ladder rung; parsers join `type=='text'` blocks. |
| Mistral | model-provider | eval plane via LiteLLM (`mistral-large-latest`); judge frontier anchor | day-1 | Third ladder rung; per-instance-configurable judge cross-check anchor. |
| Ollama | model-provider | local plane (`gemma3:12b`/`qwen2.5:7b`/`nomic-embed-text`/`bge-reranker-v2-m3`) | day-1 | Local-first-$0 cost/locality profile; judge/synth/rewrite/embed/rerank. |
| Anthropic | model-provider | Claude-Code-plane Max subscription (`claude-provider.sh anthropic`) | day-1 | Fallback rung; per-token spend policy is configuration, not hardcoded. |
| OpenAI | model-provider | provenance-only: retired usage poller; SDK adoption batch | day-1 | Fallback rung behind the gateway; enabled by configuration. |

## Observability — telemetry & liveness sinks (REQ-816..820)

The "no data is a problem, not no-problem" plane: metrics with `absent()`-guarded staleness, default-on tracing, and an external dead-man.

| connector | module surface | predecessor integration | day-1 tier | notes |
|---|---|---|---|---|
| Prometheus (metrics) | observability | textfile-collector writers + PrometheusRules; control-plane series | day-1 | Exposition/scrape of control-plane + per-connector metrics; each series freshness-stamped; `absent()`-guarded staleness. |
| Grafana | observability | `grafana.example.net`; version-controlled dashboards (`grafana/*.json`) | day-1 | Dashboards provisioned from version control; no hand-edited panels. |
| OpenObserve | observability | `dc1openobserve01:5080` OTLP traces (pinned v0.91.1) | day-1 | Default-on OTLP trace/log export; session trajectory reconstructable. |
| Langfuse | observability | Langfuse v2 on `dc1openobserve01:3000` | day-1 | Per-session LLM/agent traces keyed by session id. |
| Healthchecks.io | observability | self-hosted on `dc1openobserve01:8000`; dead-man/ping monitoring | day-1 | External dead-man ping on each control-plane heartbeat, independent of the internal alert path. |

---

## Day-1 fleet count

**20 day-1 connectors** across seven surfaces (4 ingest, 1 tracker, 3 notifier+approval, 1 cmdb, 4 actuation, 7 model-provider, 5 observability — the two LibreNMS instances share one implementation) plus **6 reference connectors** (3 tracker, 3 notifier). The executable obligations are **REQ-801..820** in [spec/008-connectors](../spec/008-connectors/); every connector owns one task under `modules/<surface>/<connector>/` in that spec's `tasks.json`.
