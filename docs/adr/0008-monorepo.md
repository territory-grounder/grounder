# ADR 0008 — Repository layout: monorepo

## Status
Accepted.

## Context
TG is many cooperating services — the Go control-plane / agent loop / adapters (ADR-0001), the bundled LiteLLM model-gateway (ADR-0004), out-of-process modules + SDK (ADR-0005), and the `frontend/` console (ADR-0006) — packaged as one guided **docker-compose** single-node profile [R corrections]. The audit demands **single-source-of-truth generation**: every wire contract, DDL, validator, human-facing count, and diagram is generated from one authoritative entity definition and CI fails on drift (INV-15) [O]; there is one implementation of each pipeline stage / alert-source type, no per-site fork (INV-18) [O]; and the safety-critical Go files are bound to their EARS specs by a content-hash lockstep manifest (spec/007) [F][R rule 10]. All of these are far cheaper to enforce atomically.

## Decision
Ship TG as a **monorepo** — **one repo, many images** [R corrections/identity] — under GitLab group `products/territory-grounder`, flagship project `grounder` (CLI `grounder`, alias `tg`) [R corrections].
- Layout includes `adapters/` (module interfaces), `modules/` (implementations + reference set + SDK) [R], `frontend/` [R], the Go control-plane/agent, migrations, generated contracts, and `docs/` (this ADR set, `docs/the-map-is-not-the-territory.md` manifesto) [R].
- A single CI pipeline runs the cross-cutting gates in one place: the generate-and-diff drift gate (INV-15), the `sh -c`/string-SQL/retired-identifier grep gates (INV-02/03/17), the spec↔Go lockstep drift guard (spec/007), and the adversarial boundary-coverage suite (INV-22) [O].

## Consequences
- Atomic changes across control-plane + contract + frontend + adapter land in one MR — a security fix cannot land on one surface and silently miss another (the INV-18 [O] anti-drift lesson generalized to the whole tree).
- "One repo, many images" keeps the docker-compose profile coherent while each service still builds and deploys independently.
- The public **module SDK will be split into its own versioned artifact later** [R corrections] once its interface stabilizes, so third-party module authors depend on a small semver'd package, not the whole tree.

## Alternatives
- **Polyrepo** (per-service repos) — rejected initially: cross-cutting single-source generation (INV-15) and spec-lockstep drift guards become cross-repo coordination problems; versioning churn slows the greenfield build.
- **Monorepo forever, never split the SDK** — rejected: third-party module authors should not vendor the whole product; hence the planned SDK extraction above.
