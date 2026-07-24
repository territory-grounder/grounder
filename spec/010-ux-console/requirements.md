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
  WHILE the control-plane reports `mutation_enabled = false`, the console SHALL render investigation,
  timeline, and audit views in read-only form and SHALL NOT present any mutating control (approve, veto,
  band-change, kill-switch, or administrative write).

- **REQ-603** — [R] corrections (UX/UI) · [O] INV-09.
  WHEN the control-plane reports `mutation_enabled = true` AND the authenticated caller holds the RBAC
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

## API-first contract

The console imports one generated TypeScript client produced from the authoritative OpenAPI document
(INV-15). A second, hand-written wire contract is forbidden; CI fails on any hand-authored endpoint URL
or request/response type in `frontend/`. TanStack Query owns server-state caching over that client, and
the SSE channel (REQ-613) is the only push path.

## Read-only-to-operational boundary

The console has exactly two authority states, both decided by the server. WHILE `mutation_enabled` is
false (Phase 0–1) the console is read-only (REQ-602). WHEN `mutation_enabled` is true (Phase 2) an
operational control becomes available only when the server grants the caller's RBAC role for it
(REQ-603), and every mutating action re-checks authority server-side (REQ-605, REQ-610). The browser
never grants a control the API denied (REQ-615), and every error path renders the restrictive state
(REQ-616).
