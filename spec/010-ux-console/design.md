<!-- spec/010 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/010 — Design: the operator console

How the requirements in `requirements.md` are realized on the Go / Temporal / PostgreSQL control-plane
and the React / Vite / TypeScript `frontend/`. Where this design and the code disagree, the code is the
bug and this document is the intent.

## Stack and boundary

Per [ADR-0011](../../docs/adr/0011-frontend-stack-react-vite.md), `frontend/` is **React + TypeScript**
built with **Vite**, styled with **Tailwind CSS + shadcn/ui** (Radix primitives, vendored into the repo
so components are fully ownable and accessible by construction), with **TanStack Query** for server
state and **TanStack Table** for the data-dense grids (approval feed, ledger, manifests). Real-time is
**Server-Sent Events**; visualization uses a headless charting layer for the manifest timeline and the
confidence trajectory.

The console is a pure read/steer surface over the Go control-plane. It owns **no** decision logic: the
band, the verification verdict, the never-auto floor, and every authorization grant are computed and
enforced server-side (INV-08, INV-09). The browser renders those decisions and issues intents; the
server is the only authority (REQ-615). This is the load-bearing design choice — a compromised or buggy
browser can never widen autonomy, because the effect authority lives in the control-plane, not the SPA.

## API-first: one generated client (REQ-601, INV-15)

`frontend/src/api/generated-client.ts` is emitted from the authoritative OpenAPI document — the same
single-source-of-truth that generates the DDL, validators, and count blocks (INV-15). No hand-written
request/response type or endpoint URL exists in `frontend/`; a CI lint fails the build on one. TanStack
Query wraps the generated client so caching, retries, and invalidation are uniform, and the query keys
are derived from the generated operation ids. A drift between the running API and the client is a CI
failure, not a runtime surprise — the UI, the API, and the running system cannot disagree.

## The two authority states (REQ-602, REQ-603, REQ-616)

The control-plane exposes `mutation_enabled` and a per-caller capability set on a session-bootstrap
endpoint. `frontend/src/mode/mode-provider.tsx` reads both and drives a `MutationMode` context. WHILE
`mutation_enabled = false` (Phase 0–1) the provider renders every panel in its read-only projection and
no mutating control is mounted (REQ-602). WHEN `mutation_enabled = true` (Phase 2) a control mounts only
if the capability set grants the caller's RBAC role for it (REQ-603). Every API error or authorization
denial resolves to the restrictive projection via `frontend/src/lib/guarded-state.ts` — the default arm
of the fetch state machine is "no data, no control", never optimistic (REQ-616).

## Panels

- **Approval console** (`frontend/src/panels/approval/`, REQ-604, REQ-605). The pending-decision feed is
  a TanStack Query subscription refreshed by the SSE `approval` event. Each `DecisionCard` renders the
  proposed plan (its two-or-more approaches), the committed `plan_hash`-keyed prediction, and the
  reversibility / blast-radius signals. Approve / veto / handoff post through the generated client to a
  `SignalWithStart`-backed endpoint that re-checks the caller's RBAC role and on-call assignment
  server-side (INV-01); the button is presentational only and is absent for a non-approver. Authority is
  the tenant-free RBAC approver graph — roles plus the on-call rotation — never a named person.

- **ActionManifest timeline / replay** (`frontend/src/panels/manifest/`, REQ-606, REQ-607, INV-07). The
  timeline renders the predicted → approved → executed → verified stages of a single content-hashed
  `ActionManifest` as one visual chain keyed by `action_id`. Every stage node asserts the same
  `action_id`; a differing id renders a mismatch banner rather than a continuous chain. Replay
  reconstructs the chain from persisted governance records through read-only endpoints and never calls
  an execution path — replay is reconstruction, not re-run.

- **Tamper-evident ledger view** (`frontend/src/panels/ledger/`, REQ-608, INV-19). A virtualized
  TanStack Table over the append-only hash-chained `governance_ledger`. The chain-verification verdict
  is the server-side `LedgerVerifier`'s output, surfaced verbatim; a broken chain renders a
  tamper-detected state. The browser never re-walks or recomputes the chain — "who let the agent act,
  and why" is proven by the server, displayed by the client.

- **Explainability** (`frontend/src/panels/explain/`, REQ-609, INV-11). "Why did the agent do this":
  the retrieval context, the risk band and its signals, the execution class, the confidence trajectory,
  and the orchestrator-captured `ToolResult` evidence IDs that backed any auto-resolve — all read from
  governance persistence, so the explanation is the audited record, not a re-narration by the model.

- **Autonomy-band + kill-switch controls** (`frontend/src/panels/controls/`, REQ-610, REQ-611, INV-21).
  The on/off and DARK → SHADOW → ENFORCE control for every autonomy layer, each independently
  disableable. These controls write to the policy store through the API and are audited on change; they
  do **not** touch a host-local sentinel file — the mechanism moved from `touch`/`rm` to an RBAC-gated,
  audited API/config control. A kill-switch renders disabled only after the API confirms the write; an
  unconfirmed or failed request keeps the prior state (REQ-611).

- **Admin** (`frontend/src/panels/admin/`, REQ-612, INV-01). Users, RBAC role assignments, the on-call
  rotation and escalation policy, and per-module enablement, all written through the generated client
  and gated to the administrator role; the panel is absent for a non-administrator.

## Real-time (REQ-613)

A single SSE connection carries session activity, approval prompts, and ledger updates
(`frontend/src/api/sse.ts`). On disconnect the console shows a disconnected indicator and reconnects with
backoff; TanStack Query invalidations are keyed off the SSE event types so a reconnect re-hydrates state.
Where a bidirectional channel is later needed, ADR-0011 permits WebSocket, but the read/steer model
keeps SSE sufficient for the panels above.

## Accessibility (REQ-614)

WAI-ARIA is a construction property, not a retrofit: Radix primitives supply roles, names, and states;
`frontend/src/a11y/baseline.ts` enforces focus-visible management, keyboard operability of every
interactive element, and a WCAG 2.1 AA contrast floor across light and dark themes. The a11y baseline is
a tracked deliverable of the scaffold task, exercised by the acceptance oracle.

## Out of scope

The OpenAPI document itself, the `LedgerVerifier`, the risk banding, the prediction/verdict, and the
policy store are owned by other specs (spec/001, spec/002, spec/006) and the control-plane; this spec
owns only their rendering and the intents the console issues back through the generated client.
