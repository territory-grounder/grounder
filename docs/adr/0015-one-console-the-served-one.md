# ADR-0015 — One console: the served one. The React frontend/ is removed.

- **Status:** Accepted (2026-07-30)
- **Supersedes:** ADR-0006 (frontend TypeScript) and ADR-0011 (React + Vite) **as they applied to a
  separate SPA** — the console remains TypeScript-in-the-browser, but as the assembled
  `deploy/console/v2/` artifact, not a parallel React application.

## Context

Two console implementations coexisted for 13 days. `deploy/console/v2/` is served in production
(22 surfaces over authenticated HTTPS, byte-verified by `make console-verify`, guarded by
Playwright oracles and `deploy/served_console_test.go`). The React/Vite `frontend/` was unreachable
by construction: the console image overwrote its entry point; nginx logs showed its bundle
referenced 0 times and its 973 KB of assets requested 0 times — while its CI job stayed green and
all 8 spec/010 tasks certified it as "completed". The unreachable half consumed 91 path-touches,
misdirected four audit sweeps, and made the component's tested% describe a program no one runs.

## Decision

- `frontend/` is deleted. The tree as it existed is tagged **`archive/frontend-react`**.
- spec/010 certifies only the served artifact (`deploy/console/v2/index.html`), bound by T-010-9
  and `deploy/served_console_test.go` (which fails if the Dockerfile is ever re-pointed without
  the spec saying so).
- **No parallel console implementations, ever again.** Console work WIRES the served artifact.
  A rewrite proposal must first remove the served console in the same MR that replaces it — there
  is no "build alongside, switch later" lane; that lane is where 13 days went.

## Consequences

- Rebuilding on React later means starting from `archive/frontend-react` **and** honoring the
  replace-in-one-MR rule above.
- The `frontend` CI job is removed; console gates are `console-verify`, `console-e2e`, and the
  served-console tests.
