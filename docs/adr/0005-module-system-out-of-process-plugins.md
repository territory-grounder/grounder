# ADR 0005 — Module system: out-of-process governed plugins

## Status
**PROPOSED.**

## Context
Every integration surface — ingest / tracker / notifier+approval / CMDB / actuation / model-provider / observability — must be a **loadable/unloadable module**, not a hardcoded vendor [R corrections][R paradigm-rule 3]. Two hard constraints frame the design. (1) The audit's INV-17 [O]: *a capability exists only if its adapter is compiled in and explicitly registered; there is no runtime "mode" string, no host trust path for an unregistered backend, and retiring a capability = deleting its package* — because the predecessor's "retired" OpenClaw path stayed live and executable (H-08, M-12) [O]. (2) Multi-tenancy [R rule 1]: modules are enabled and capability-scoped **per tenant**, and no module may cross a tenant boundary.

## Decision
**Default mechanism: out-of-process governed plugins.** Each module is a separate process/container over a stable protocol — gRPC / HashiCorp `go-plugin`; **MCP** for tool/actuation modules [R corrections].
- Gives runtime **load/unload**, **third-party** modules, **process isolation**, and **per-module + per-tenant capability scoping**.
- **Governed by construction** — modules are **signed**, **capability-scoped**, and **per-tenant-enabled**; a disabled or unregistered module has **NO execution path**. This reconciles with INV-17 [O]: registration is the *governed* analogue of "compiled in" — a startup reconciler compares live modules against a **signed declared manifest** and refuses to start on mismatch, and adapters are unexported behind the Go interceptor chain (INV-21) [O]. The "dead OpenClaw path still executable" class dies.
- Repo layout: `adapters/` = the module **interfaces**; `modules/` = loadable **implementations** + a small reference-adapter set + an **SDK** [R corrections].
- Every actuation module traverses the service-side per-tenant pre-execution gate (territory/egress/policy → execute → audit) inside the orchestrator (INV-21) [O][R rule 4].

## Consequences
- Process isolation means a hostile/buggy third-party module cannot corrupt control-plane memory; blast radius is one process, healed by the residual platform-controller (ADR-0002) [R].
- The **signed-manifest reconciler** is now a first-class safety control, not just registration — it enforces the INV-17 guarantee for a *dynamic* plugin set.
- Slightly higher operational surface (N processes) than in-process registration; accepted for isolation + third-party extensibility.

## Alternatives
- **B — compile-time registry** (Go interfaces resolved from an import-populated registry, exactly INV-17's literal enforcement) — simplest and strongest for the built-in reference set, but **no runtime load/unload and no third-party modules** without a rebuild. Retained as the fallback if out-of-process proves too heavy.
- **C — WASM / Extism** — strong sandboxing and portability, but immature host-call ergonomics for the gRPC/MCP actuation surface and streaming tool-results. Deferred.
