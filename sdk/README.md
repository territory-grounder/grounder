# `sdk/` — client + module-author SDK

The importable Go client for the Territory Grounder API and the SDK for authoring third-party
**modules** (the governed plugin contract for `modules/`). This is the "governance layer as an
importable package" that ADR-0008 anticipates splitting into its own repo once an external consumer
needs to version it independently.

**Status:** stub; grows alongside the module runtime (P1-3) and the generated OpenAPI (P1-11).
