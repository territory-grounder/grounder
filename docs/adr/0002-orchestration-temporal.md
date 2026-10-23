# ADR 0002 — Orchestration substrate: Temporal

## Status
Accepted.

## Context
The predecessor ran the incident lifecycle as a 51-node n8n Runner workflow, a 73-node Matrix Bridge, ~14 receiver workflows, a Cronicle scheduler, and a `trap EXIT` gateway-watchdog dead-man script [F]. That stack coupled orchestration to a low-code engine, forked logic per site (INV-18) [O], embedded credentials in exportable JSON (INV-13) [O], and let a "non-bypassable" gate be walked past because ordering lived in node wiring, not code (H-02) [O]. TG needs durable pause/resume for async human approval [F], per-turn immutable snapshots, slots→scalable concurrency, and native scheduling with per-job run history.

## Decision
Adopt **Temporal** (durable workflows + activities + Schedules + signals + continue-as-new) as the single, deliberately load-bearing orchestration substrate [R corrections/stack].
- Temporal workflows are the natural home for the **deterministic orchestrator** [F]; workflow code contains decision logic only and **cannot touch the OS** — every side effect is an activity against a capability-scoped adapter (INV-02/21) [O].
- Ordering (`Predict → Approval → Execute → Verify`) is enforced by workflow structure (INV-10) [O]; a poll activity cannot start without a persisted prediction — the fail-closed gate becomes structural, killing H-02/H-03 [O].
- Async approval is a **Temporal signal** on durable pause/resume state (INV-12) [O]; re-engagement mints a *new* workflow re-running the full gate — there is no coding-CLI session-resume primitive to hijack [O H-01].
- **Runtime substrate migration** [R]: Temporal **replaces** n8n (engine), **Cronicle** (→ Temporal Schedules: run-history/retries/dead-man native), and **most of the watchdog/reconcile** machinery.

## Consequences
- **Concentration risk**: Temporal becomes single-point-critical. Mitigated by (a) the residual platform-controller healing non-Temporal things — LiteLLM gateway, dead module process, stuck PG pool (ADR-0004/0005) [R]; (b) keeping the `absent()` "no-data ≠ no-alert" principle over worker/workflow health [R paradigm-rule 4][F]; (c) registering every workflow/activity in the self-populating liveness registry (INV-17) [O][R rule 9].
- Sessions are isolated executions keyed `tg/{tenant}/{session_id}`; cancel = `TerminateWorkflow(id)` — no process-wide `pkill` (INV-12) [O].
- Fixed 5 slots [F] → workers/task-queues + per-tenant configurable concurrency + fair-share [R].

## Alternatives
- **Keep n8n + Cronicle** [F] — rejected [R rule 7]: the wiring-not-code ordering enabled the crown-jewel bypass (H-02) [O].
- **Bespoke Go state machine on Postgres** — rejected: re-implements durable timers, retries, replay, and signals that Temporal provides GA; higher risk for the safety spine.
- **Cadence / Argo Workflows** — rejected: Temporal's Go SDK maturity (ADR-0001) and signal/continue-as-new model fit pause/resume best.
