# `modules/` — loadable integration modules

Loadable, unloadable **implementations** of the `adapters/` interfaces (ingest / tracker /
notifier+approval / CMDB / actuation / model-provider / observability), plus a small reference set
and the module-author SDK.

**Mechanism (ADR-0005, proposed):** out-of-process governed plugins — each module a separate
process/container over gRPC / go-plugin, with **MCP** for tool/actuation modules. Modules are
**signed, capability-scoped, per-tenant-enabled**; a disabled/unregistered module has **no execution
path** (kills the "dead OpenClaw path still executable" class). [O] INV-17; [R] paradigm-rule 3.

**Status:** the runtime is **P1-3** (see `docs/EXECUTION-PLAN.md`). `adapters/` holds the interfaces.
