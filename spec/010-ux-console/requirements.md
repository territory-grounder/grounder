<!-- spec/010 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/010 — Operator console (UX pillar)

**Owning behavior family:** Track A (UX / console) — see [`docs/ROADMAP.md`](../../docs/ROADMAP.md) and
[`docs/adr/0011-frontend-stack-react-vite.md`](../../docs/adr/0011-frontend-stack-react-vite.md).
**Constitution / invariants:** INV-01, INV-07, INV-08, INV-09, INV-11, INV-15, INV-19, INV-21.
**Phase:** the read-only console (audit / timeline / ledger view) lands in Phase 0–1; the operational
controls (approval, band + kill-switch, admin) land in Phase 2 as mutation is earned. **Status:** Approved.

The console is a **named first-class product pillar** — the surface where governed autonomy becomes
visible, steerable, and auditable. It is built **API-first**: it consumes only the single generated
OpenAPI client (INV-15), never a second hand-written contract. The console holds no safety authority of
its own — every band, verdict, floor, and authorization decision is the server's, and the browser
renders it. It is single-organization with RBAC roles (approver, operator, administrator, viewer) and an
on-call rotation; there is no tenant selector. This document is the requirement source of record; the
design is in `design.md`, the runnable acceptance oracles are in `acceptance/`, and the engineering
tasks are in `tasks.json`.

## Requirements

- **REQ-601** — [O] INV-15 · [R] corrections (UX/UI).
  The console SHALL construct its entire API layer from the single generated OpenAPI client, and SHALL
  NOT declare a hand-written request type, response type, or endpoint URL for any control-plane call.

- **REQ-602** — [O] INV-09 · [O] Phase 0.
  WHILE the control-plane reports `may_actuate = false` (the owner-set mode withholds actuation), the
  console SHALL render investigation, timeline, and audit views in read-only form and SHALL NOT present
  any mutating control (approve, veto, band-change, kill-switch, or administrative write).

- **REQ-603** — [R] corrections (UX/UI) · [O] INV-09.
  WHEN the control-plane reports `may_actuate = true` AND the authenticated caller holds the RBAC
  role the API requires for a control, the console SHALL enable that operational control; otherwise the
  console SHALL keep the control disabled.

- **REQ-604** — [F] Phase 6 approve · [R] paradigm-rule 2.
  WHEN the approval feed contains a pending POLL_PAUSE or AUTO_NOTICE decision, the console SHALL
  display, for that decision, the proposed plan with its two-or-more approaches, the committed machine
  prediction, and the reversibility and blast-radius signals.

- **REQ-605** — [O] INV-01 · [F] Phase 6 approve.
  WHEN an approver approves, vetoes, or hands off a pending decision, the console SHALL issue the action
  through the generated client so the API re-checks the caller's RBAC role and on-call assignment
  server-side, and SHALL treat the server authorization result as final. IF the authenticated caller
  lacks the required approver role, THEN the console SHALL present no approve, veto, or handoff control
  for that decision.

- **REQ-606** — [O] INV-07 · [R] corrections (UX/UI).
  WHEN an operator opens an `ActionManifest`, the console SHALL render its predicted → approved →
  executed → verified stages as one visual chain keyed by the single content-hashed `action_id`, and
  SHALL display a mismatch state IF any stage carries a different `action_id`.

- **REQ-607** — [R] corrections (UX/UI).
  WHEN an operator replays a completed `ActionManifest`, the console SHALL reconstruct the chain from
  persisted governance records through the generated client and SHALL NOT trigger any re-execution of
  the action.

- **REQ-608** — [O] INV-19 · [F] governance hash-chain.
  WHEN the console renders the governance ledger, it SHALL display the chain-verification status
  returned by the server-side `LedgerVerifier`, SHALL present a tamper-detected state WHEN the verifier
  reports a broken chain, and SHALL NOT compute the chain verdict in the browser.

- **REQ-609** — [O] INV-11.
  WHEN an operator opens the explainability view for a session, the console SHALL show the retrieval
  context, the risk band with its signals, the execution class, the confidence trajectory, and the
  orchestrator-captured `ToolResult` evidence IDs that backed any auto-resolve.

- **REQ-610** — [R] paradigm-rule 4 · [O] INV-21.
  WHEN an administrator changes an autonomy-band control or a per-layer kill-switch (off / DARK / SHADOW
  / ENFORCE), the console SHALL apply the change through the policy API so it is stored as RBAC-gated
  organization policy and audited on change, and SHALL NOT read or write a host-local file to effect the
  control. Each autonomy layer SHALL be independently disableable from the console.

- **REQ-611** — [O] INV-09.
  WHEN an administrator activates a kill-switch, the console SHALL reflect the disabled state only after
  the API confirms it, and SHALL NOT display the control as disabled on an unconfirmed or failed request.

- **REQ-612** — [R] paradigm-rule 2 · [O] INV-01.
  WHERE the authenticated caller holds the administrator role, the console SHALL provide management of
  users, RBAC role assignments, the on-call rotation and escalation policy, and per-module enablement,
  each written through the generated client; WHERE the caller lacks the administrator role, the console
  SHALL NOT expose the admin panel.

- **REQ-613** — [R] ADR-0011 · [O] INV-15.
  The console SHALL receive live session activity, approval prompts, and ledger updates over a
  Server-Sent Events stream; WHEN the stream disconnects, the console SHALL display a disconnected
  indicator and SHALL attempt reconnection.

- **REQ-614** — [O] ADR-0011.
  The console SHALL make every interactive component keyboard-operable and expose WAI-ARIA roles, names,
  and states, SHALL maintain a visible focus indicator, and SHALL hold a contrast ratio at or above
  WCAG 2.1 AA on every panel.

- **REQ-615** — [O] INV-08 · [O] INV-09.
  The console SHALL treat each server response as the source of truth for every safety decision, and
  SHALL NOT enforce a risk band, a verification verdict, the never-auto floor, or an authorization grant
  in browser code; a control the API denies SHALL remain unavailable in the console.

- **REQ-616** — [O] INV-09.
  IF a control-plane call returns an authorization error or a transport error, THEN the console SHALL
  render the restrictive state — no data and no mutating control for the affected panel — and SHALL NOT
  fall back to an optimistic or cached-permissive view.

- **REQ-617** — [O] INV-15 · [R] a spec that certifies the unserved half of a component reports its own
  coverage as higher than it is.
  This spec SHALL name the artifact an operator is ACTUALLY SERVED, and the image SHALL serve that artifact
  and no unreachable payload beside it. **The served operator console is
  `deploy/console/v2/index.html`** — a self-contained document assembled by `deploy/console/v2/assemble.py`
  from `console.html` + `modules/*`, with its fonts inlined as `data:` URIs. It is served at the site root by
  `deploy/console/Dockerfile`. Where the served entry point is SELF-CONTAINED (it references no external
  script, stylesheet or font), the console image SHALL NOT copy any other web asset into the served document
  root: nothing an operator loads could reach it. `deploy/served_console_test.go` derives the served entry
  point from the Dockerfile — the LAST `COPY` writing `index.html` wins, as Docker layers it — and fails CI
  on either violation.
  **The `frontend/` React application was REMOVED on 2026-07-30** (ADR-0015; the tree as it existed is
  tagged `archive/frontend-react`). It was unreachable by construction — the console image overwrote its
  entry point and nginx served its bundle 0 times — while its green CI job and tasks T-010-1..8 certified
  it. Those task records are retained as history of work that was superseded, not as certification of any
  reachable surface: **the only console this spec certifies is the served artifact named above**, and
  T-010-9 plus `deploy/served_console_test.go` are the binding. Any future console rework WIRES the served
  artifact; a parallel implementation that is not served may not be added to this repository (that pattern
  cost 91 path-touches of certified-but-unreachable code and misdirected four audit sweeps).
  *Rationale (measured live 2026-07-29 on dc1tg01).* The image ran the Vite build, copied `dist/` into the
  document root, and then overwrote that build's `index.html` with the v2 console — so the React entry point
  never loaded and its bundle was unreachable by construction. In the running container: the served
  `index.html` referenced the React bundle **0 times**; nginx had served `index-*.js` / `index-*.css` **0
  times** across every request it had logged; `/assets/` (973 KB — one JS bundle, one stylesheet, 56 font
  files) had been requested **0 times**. Meanwhile **all 8** of this spec's tasks own ONLY `frontend/` paths
  and are marked completed. Every console image carried ~1 MB no operator could load, every pipeline paid for
  a node build nobody would run, and this component's tested% described a program that was never served —
  which is most of the distance between its delivered score and its operational one. Reviving the port is one
  `COPY` line plus an update here, which the guard will then demand: what is SERVED and what is CERTIFIED can
  no longer drift apart silently.

## API-first contract

The console imports one generated TypeScript client produced from the authoritative OpenAPI document
(INV-15). A second, hand-written wire contract is forbidden; CI fails on any hand-authored endpoint URL
or request/response type in `frontend/`. TanStack Query owns server-state caching over that client, and
the SSE channel (REQ-613) is the only push path.

## Read-only-to-operational boundary

The console has exactly two authority states, both decided by the server — derived from the owner-set
4-mode chokepoint (TG-112). WHILE `may_actuate` is false the console is read-only (REQ-602). WHEN
`may_actuate` is true an
operational control becomes available only when the server grants the caller's RBAC role for it
(REQ-603), and every mutating action re-checks authority server-side (REQ-605, REQ-610). The browser
never grants a control the API denied (REQ-615), and every error path renders the restrictive state
(REQ-616).
