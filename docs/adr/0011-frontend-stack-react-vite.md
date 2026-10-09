# ADR 0011 — Frontend stack: React + Vite + TypeScript + Tailwind + shadcn/ui

## Status
Accepted. Refines ADR-0006 (frontend = TypeScript; framework deferred) by pinning the framework and
component system. The UX is a **named first-class product pillar** ("best-in-class operator console"),
so the stack is chosen to maximize UI quality, not merely to have one.

## Context
Territory Grounder's console is where the differentiator becomes usable: the approval console (the human
circuit-breaker), the `ActionManifest` predicted→approved→executed→verified timeline/replay, the
tamper-evident ledger view, explainability, autonomy-band + kill-switch controls, and admin. It is
data-dense, real-time, and safety-critical — it must be exceptionally clear and hard to misuse. The
frontend is built **API-first** against the single generated OpenAPI contract (INV-15); the framework
choice is about rendering that contract into the best possible operator experience.

## Decision
The `frontend/` console is **React + TypeScript**, built with:

- **Vite** — fast dev server + build.
- **Tailwind CSS + shadcn/ui** (Radix UI primitives) — a design-system-first, **fully-ownable** component
  set (components are vendored into the repo, not a black-box dependency), accessible by construction
  (WAI-ARIA via Radix), themeable, no vendor lock.
- **TanStack Query** for server state over a **generated typed client** from the OpenAPI contract (no
  hand-written API types — INV-15), and **TanStack Table** for the data-dense grids (incidents, ledger,
  manifests).
- **Real-time** via Server-Sent Events (and WebSocket where bidirectional) for live session/agent
  activity, approval prompts, and ledger updates.
- Visualization via a headless charting layer (e.g. visx / Recharts) for the trajectory + governance
  panels; a command-palette (kbar-style) for operator speed.

Rationale for React over the alternatives: the **deepest ecosystem** for polished, data-dense operator
tooling (data grids, timelines, charts, command palettes, virtualization), the strongest
design-system + accessibility tooling (shadcn/ui + Radix), and the **largest talent pool** for an
open-source project that wants outside contributors to build an exceptional UI. shadcn/ui specifically
matches the project's "own your components / no black box" ethos.

## Consequences
- `frontend/` is a Vite + React + TS app; CI adds a frontend lint/build/test lane; the console consumes
  only the generated OpenAPI client (a second, hand-written contract is forbidden — INV-15).
- UX quality is a tracked deliverable (spec/010-ux-console), with functional acceptance criteria (the
  console renders a real manifest/ledger from the API; band/kill controls call the API and are
  RBAC-gated; read-only in Phase 0/1, operational in Phase 2).
- The design system (tokens, components, a11y baseline) is a first-class artifact, not an afterthought.

## Alternatives
- **SvelteKit** — excellent DX and smaller bundles; rejected for the shallower component/design-system
  ecosystem, which matters most for a "best-in-class" data-dense console and outside contribution.
- **SolidJS / Solid Start** — fast and elegant; rejected for the smaller community and component ecosystem.
- **Angular** — rejected as heavier than needed for this SPA.
